package postgreslog

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

// A stalled appender holds nothing another process waits on. A process can stop between any two
// of its round trips to the database (SIGSTOP, a suspended VM, a long GC pause, a partition). If
// Append left a transaction open across two round trips, a stall there would keep its locks, and
// every other process appending to the same entity would block for as long as the stall lasts.
//
// Each case runs one Append on a pool whose connection stops after its k-th statement returns, for
// k = 1, 2, ... until Append makes fewer than k statements. While it is stopped, another process
// appends to the same entity and must finish without waiting on the stopped session. The stop is
// a held connection, not a sleep, and the test reads the lock graph from pg_blocking_pids, so a
// blocked append is detected, not timed out. Skips without PG_DSN.
func TestAppend_StalledAppenderHoldsNoLock(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	other, err := Open(ctx, dsn) // the other process
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	cases := map[string]struct {
		id, event string // the other process's append
		wantSeq   int64
	}{
		"next_id": {"second", "b", 1},
		"same_id": {"first", "a", 0}, // a transport retry of the stalled append
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for k := 1; ; k++ {
				st := newStaller(dsn)
				db := sql.OpenDB(st)
				stalled, err := newLog(ctx, db)
				if err != nil {
					t.Fatal(err)
				}
				entity := fmt.Sprintf("pg-stall-%s-%d-%d", t.Name(), k, time.Now().UnixNano())
				st.arm(k)
				opDone := make(chan error, 1)
				go func() {
					_, err := stalled.Append(ctx, entity, "first", "a")
					opDone <- err
				}()
				var pid uint32
				select {
				case err := <-opDone: // Append made fewer than k statements
					db.Close()
					if err != nil {
						t.Fatalf("unstalled append: %v", err)
					}
					if k == 1 {
						t.Fatal("Append made no statement the staller saw")
					}
					return
				case pid = <-st.stalled:
				}
				afterDone := make(chan error, 1)
				go func() {
					seq, err := other.Append(ctx, entity, tc.id, tc.event)
					if err == nil && seq != tc.wantSeq {
						err = fmt.Errorf("the other process's append took position %d, want %d", seq, tc.wantSeq)
					}
					afterDone <- err
				}()
				blocked := waitDoneOrBlocked(t, admin, pid, afterDone)
				if blocked {
					t.Errorf("stalled after statement %d: the other process's append is blocked on the stalled session's lock, and stays blocked for as long as the stall lasts", k)
				}
				st.resume() // the stall ends
				if blocked {
					<-afterDone // its outcome depends on the stalled append's, which the stall decided
				}
				if err := <-opDone; err != nil {
					t.Errorf("stalled append: %v", err)
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
				t.Errorf("the other process's append, during the stall: %v", err)
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
	sent    atomic.Int64 // statements run, stalled or not
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
	s.sent.Add(1)
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
