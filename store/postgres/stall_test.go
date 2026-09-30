package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// A stalled client holds nothing another node waits on. A process can stop between any two of its
// round trips to the database (SIGSTOP, a suspended VM, a long GC pause, a partition). If a store
// operation left a transaction open across two round trips, a stall there would keep its locks,
// and every other node that touches the same lease row or run would block for as long as the stall
// lasts: the documented takeover one TTL after the last renewal would not happen.
//
// Each case below runs one store operation on a pool whose connection stops after its k-th
// statement returns, for k = 1, 2, ... until the operation makes fewer than k statements. While it
// is stopped, another node's store performs the operation that must still go through (taking the
// lease once it has expired or been released, or recording the run's next step) and must finish
// without waiting on the stopped session. The stop is a held connection, not a sleep, and the test
// reads the lock graph from pg_blocking_pids, so a blocked operation is detected, not timed out.
// Skips without PG_DSN.
func TestPostgres_StalledClientHoldsNoLock(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	other, err := Open(ctx, dsn) // the other node
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	const ttl = 500 * time.Millisecond
	type scenario struct {
		// setup runs before the stall is armed; op is the operation that stalls; after runs on the
		// other node while it is stalled and must finish.
		setup func(t *testing.T, s *Store, run string)
		op    func(s *Store, run string) error
		after func(t *testing.T, run string) error
	}
	// takeOver waits (on the database clock, which a read does not block on) until the committed
	// lease has expired, then takes it over from the other node.
	takeOver := func(waitExpiry bool) func(t *testing.T, run string) error {
		return func(t *testing.T, run string) error {
			if waitExpiry {
				if err := waitLeaseExpired(admin, run); err != nil {
					return err
				}
			}
			ok, err := other.AcquireLease(ctx, run, "taker", ttl)
			if err == nil && !ok {
				err = fmt.Errorf("AcquireLease of an expired or released lease = false")
			}
			return err
		}
	}
	cases := map[string]scenario{
		"acquire": { // a holder re-acquiring its own lease (a renewal through AcquireLease)
			setup: func(t *testing.T, s *Store, run string) { mustAcquire(t, s, run, "stalled", ttl) },
			op: func(s *Store, run string) error {
				_, err := s.AcquireLease(ctx, run, "stalled", ttl)
				return err
			},
			after: takeOver(true),
		},
		"renew": {
			setup: func(t *testing.T, s *Store, run string) { mustAcquire(t, s, run, "stalled", ttl) },
			op: func(s *Store, run string) error {
				_, err := s.RenewLease(ctx, run, "stalled", ttl)
				return err
			},
			after: takeOver(true),
		},
		"release": {
			setup: func(t *testing.T, s *Store, run string) { mustAcquire(t, s, run, "stalled", time.Minute) },
			op:    func(s *Store, run string) error { return s.ReleaseLease(ctx, run, "stalled") },
			after: takeOver(false),
		},
		"insert": {
			setup: func(t *testing.T, s *Store, run string) {},
			op: func(s *Store, run string) error {
				_, _, err := s.Insert(ctx, run, "first", []byte("a"))
				return err
			},
			after: func(t *testing.T, run string) error {
				e, inserted, err := other.Insert(ctx, run, "second", []byte("b"))
				if err == nil && (!inserted || e.Seq != 1) {
					err = fmt.Errorf("Insert of the run's second step = (%+v, %v), want inserted at seq 1", e, inserted)
				}
				return err
			},
		},
		"insert_same_name": {
			setup: func(t *testing.T, s *Store, run string) {},
			op: func(s *Store, run string) error {
				_, _, err := s.Insert(ctx, run, "step", []byte("a"))
				return err
			},
			after: func(t *testing.T, run string) error {
				e, inserted, err := other.Insert(ctx, run, "step", []byte("b"))
				if err == nil && (inserted || string(e.Data) != "a") {
					err = fmt.Errorf("Insert of a name the stalled node recorded = (%+v, %v), want its entry", e, inserted)
				}
				return err
			},
		},
	}
	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			for k := 1; ; k++ {
				st := newStaller(dsn)
				db := sql.OpenDB(st)
				s, err := New(ctx, db)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				run := uniqueID(t, fmt.Sprintf("pg-stall-%d-", k))
				sc.setup(t, s, run)
				st.arm(k)
				opDone := make(chan error, 1)
				go func() { opDone <- sc.op(s, run) }()
				var pid uint32
				select {
				case err := <-opDone: // the operation made fewer than k statements
					db.Close()
					if err != nil {
						t.Fatalf("unstalled operation: %v", err)
					}
					if k == 1 {
						t.Fatal("the operation made no statement the staller saw")
					}
					return
				case pid = <-st.stalled:
				}
				afterDone := make(chan error, 1)
				go func() { afterDone <- sc.after(t, run) }()
				blocked := waitDoneOrBlocked(t, admin, pid, afterDone)
				if blocked {
					t.Errorf("stalled after statement %d: the other node is blocked on the stalled session's lock, and stays blocked for as long as the stall lasts", k)
				}
				st.resume() // the stall ends
				if blocked {
					if err := <-afterDone; err != nil {
						t.Errorf("after the stall ended: %v", err)
					}
				}
				if err := <-opDone; err != nil {
					t.Errorf("stalled operation: %v", err)
				}
				db.Close()
			}
		})
	}
}

// waitDoneOrBlocked returns false when done yields (failing the test on its error), and true as
// soon as Postgres reports a session waiting on a lock held by pid.
func waitDoneOrBlocked(t *testing.T, admin *sql.DB, pid uint32, done <-chan error) bool {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the other node's operation, during the stall: %v", err)
			}
			return false
		case <-tick.C:
		}
		var blocked bool
		if err := admin.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE $1::int = ANY(pg_blocking_pids(pid)))`, int64(pid)).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return true
		}
	}
}

// waitLeaseExpired polls the database clock until run's committed lease has expired.
func waitLeaseExpired(admin *sql.DB, run string) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var expired bool
		if err := admin.QueryRowContext(context.Background(), `SELECT expiry < now() FROM bide_leases WHERE run_id = $1`, run).Scan(&expired); err != nil {
			return err
		}
		if expired {
			return nil
		}
		<-tick.C
	}
}

func mustAcquire(t *testing.T, s *Store, run, holder string, ttl time.Duration) {
	t.Helper()
	if ok, err := s.AcquireLease(context.Background(), run, holder, ttl); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
}

// staller is a driver.Connector over pgx whose connections, once armed with k, stop after the
// k-th statement they run returns (an Exec, or a Query once its first result is in), until resume.
// The statement has run on the server; what stops is the client, before its next round trip.
type staller struct {
	dsn     string
	mu      sync.Mutex
	left    int           // statements before the stall; 0 when disarmed
	stalled chan uint32   // the stopped session's backend pid
	release chan struct{} // closed by resume
	resumed atomic.Bool
}

func newStaller(dsn string) *staller {
	return &staller{dsn: dsn, stalled: make(chan uint32, 1), release: make(chan struct{})}
}

func (s *staller) arm(k int) {
	s.mu.Lock()
	s.left = k
	s.mu.Unlock()
}

func (s *staller) resume() {
	if s.resumed.CompareAndSwap(false, true) {
		close(s.release)
	}
}

// after runs once a statement returned on c: at the armed statement it reports the session and
// waits for resume.
func (s *staller) after(c *stdlib.Conn) {
	s.mu.Lock()
	stop := false
	if s.left > 0 {
		s.left--
		stop = s.left == 0
	}
	s.mu.Unlock()
	if stop {
		s.stalled <- c.Conn().PgConn().PID()
		<-s.release
	}
}

func (s *staller) Connect(ctx context.Context) (driver.Conn, error) {
	c, err := stdlib.GetDefaultDriver().Open(s.dsn)
	if err != nil {
		return nil, err
	}
	return &stallConn{Conn: c.(*stdlib.Conn), s: s}, nil
}

func (s *staller) Driver() driver.Driver { return stdlib.GetDefaultDriver() }

// stallConn forwards to pgx's connection and calls its staller after each statement.
type stallConn struct {
	*stdlib.Conn
	s *staller
}

func (c *stallConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.Conn.ExecContext(ctx, query, args)
	if err == nil {
		c.s.after(c.Conn)
	}
	return r, err
}

func (c *stallConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	r, err := c.Conn.QueryContext(ctx, query, args)
	if err == nil {
		c.s.after(c.Conn)
	}
	return r, err
}
