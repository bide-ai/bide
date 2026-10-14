package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func tempJournal(t *testing.T) string { return filepath.Join(t.TempDir(), "j.db") }

// A table prefix must be an SQL identifier, and a nil option is a config error, for Open,
// OpenReadOnly and New alike.
func TestOptions_RefuseABadPrefixAndANilOption(t *testing.T) {
	for _, prefix := range []string{"", "1abc", "a-b", "a b", "a;drop", `a"b`, "é"} {
		if _, err := Open(":memory:", WithTablePrefix(prefix)); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "not an SQL identifier") {
			t.Errorf("Open(prefix %q) = %v, want ErrConfig naming the prefix rule", prefix, err)
		}
	}
	if _, err := Open(":memory:", nil); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "nil option") {
		t.Errorf("Open(nil option) = %v, want ErrConfig", err)
	}
	if _, err := OpenReadOnly(tempJournal(t), WithTablePrefix("9")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("OpenReadOnly(bad prefix) = %v, want ErrConfig", err)
	}
	if _, err := New(context.Background(), &sql.DB{}, nil); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("New(nil option) = %v, want ErrConfig", err)
	}
	for _, ok := range []string{"p", "_p", "P1_", "bide_x"} {
		s, err := Open(":memory:", WithTablePrefix(ok))
		if err != nil {
			t.Errorf("Open(prefix %q) = %v, want success", ok, err)
			continue
		}
		s.Close()
	}
}

// A file that is not a SQLite database is refused by Open with the hint, and its bytes are left alone.
func TestOpen_RefusesAFileThatIsNotADatabase(t *testing.T) {
	path := tempJournal(t)
	text := []byte("this is not a database, it is just text, padded to be long enough for sqlite to look at")
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	if !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Fatalf("Open(text file) = %v, want ErrStorage saying the file is not a SQLite database", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(text) {
		t.Error("Open modified a file that is not a database")
	}
}

func setVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE bide_schema_version SET version = ?`, v); err != nil {
		t.Fatal(err)
	}
}

// A journal written by a newer schema is refused rather than read as if it were this version's.
func TestOpen_RefusesANewerSchema(t *testing.T) {
	path := tempJournal(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	setVersion(t, path, schemaVersion+1)
	for name, open := range map[string]func() (*Store, error){
		"Open":         func() (*Store, error) { return Open(path) },
		"OpenReadOnly": func() (*Store, error) { return OpenReadOnly(path) },
	} {
		_, err := open()
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "newer than this version's") {
			t.Errorf("%s(newer schema) = %v, want ErrConfig naming both versions", name, err)
		}
	}
}

func TestOpenReadOnly_Refusals(t *testing.T) {
	for _, p := range []string{":memory:", "file:x?mode=memory&cache=shared"} {
		if _, err := OpenReadOnly(p); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "nothing to read") {
			t.Errorf("OpenReadOnly(%q) = %v, want ErrConfig", p, err)
		}
	}
	// A SQLite database that holds no bide journal is not one to inspect.
	path := tempJournal(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE other (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = OpenReadOnly(path)
	if !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "no bide journal in it") {
		t.Errorf("OpenReadOnly(foreign database) = %v, want ErrStorage saying it holds no bide journal", err)
	}
	// A prefix that names other tables finds nothing in a real journal.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenReadOnly(path, WithTablePrefix("other_")); !errors.Is(err, agent.ErrStorage) {
		t.Errorf("OpenReadOnly(wrong prefix) = %v, want ErrStorage", err)
	}
}

func walDB(t *testing.T, path string, maxConns int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(maxConns)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNew_Refusals(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, nil); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "nil db") {
		t.Errorf("New(nil) = %v, want ErrConfig", err)
	}
	if _, err := New(ctx, walDB(t, tempJournal(t), 1)); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "at least 2") {
		t.Errorf("New(one connection) = %v, want ErrConfig asking for two", err)
	}
	// A database in rollback-journal mode is refused: the store relies on WAL.
	del, err := sql.Open("sqlite", tempJournal(t)+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		t.Fatal(err)
	}
	defer del.Close()
	if _, err := New(ctx, del); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"delete"`) {
		t.Errorf("New(DELETE mode) = %v, want ErrConfig naming the journal mode", err)
	}
	// A db the caller already closed cannot report its mode.
	closed := walDB(t, tempJournal(t), 4)
	closed.Close()
	if _, err := New(ctx, closed); !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "read journal mode") {
		t.Errorf("New(closed db) = %v, want ErrStorage", err)
	}
	// migrate's refusals reach New too.
	path := tempJournal(t)
	s, err := New(ctx, walDB(t, path, 4))
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	setVersion(t, path, schemaVersion+1)
	if _, err := New(ctx, walDB(t, path, 4)); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("New(newer schema) = %v, want ErrConfig", err)
	}
}

// New works over the caller's db, honours the prefix, and leaves the db open on Close.
func TestNew_StoresOverTheCallersDB(t *testing.T) {
	ctx := context.Background()
	db := walDB(t, tempJournal(t), 4)
	s, err := New(ctx, db, WithTablePrefix("app_"))
	if err != nil {
		t.Fatal(err)
	}
	e, fresh, err := s.Insert(ctx, "r", "a", []byte("1"))
	if err != nil || !fresh || e.Seq != 1 {
		t.Fatalf("Insert = %+v %v %v", e, fresh, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM app_steps WHERE run_id = 'r'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("app_steps rows = %d, %v; want the step in the prefixed table", n, err)
	}
	if ok, err := s.AcquireLease(ctx, "r", "h", time.Minute); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Errorf("Close closed a db the caller owns: %v", err)
	}
	// A second Store over the same db and prefix sees the first one's step.
	s2, err := New(ctx, db, WithTablePrefix("app_"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s2.Get(ctx, "r", "a"); err != nil || !ok {
		t.Errorf("Get through a second New = %v, %v", ok, err)
	}
}

// Every operation on a closed Store fails with an error wrapping agent.ErrStorage, instead of
// reporting an empty journal or a granted lease.
func TestClosedStore_FailsWithErrStorage(t *testing.T) {
	ctx := context.Background()
	for name, open := range map[string]func() *Store{
		"file": func() *Store {
			s, err := Open(tempJournal(t))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"memory": func() *Store {
			s, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	} {
		s := open()
		if err := s.Close(); err != nil {
			t.Fatalf("%s: Close: %v", name, err)
		}
		es := s.EventStore()
		check := func(op string, err error) {
			t.Helper()
			if !errors.Is(err, agent.ErrStorage) {
				t.Errorf("%s: %s on a closed store = %v, want ErrStorage", name, op, err)
			}
		}
		_, _, err := s.Insert(ctx, "r", "a", []byte("x"))
		check("Insert", err)
		_, _, err = s.Get(ctx, "r", "a")
		check("Get", err)
		for _, err := range s.Load(ctx, "r", 0) {
			check("Load", err)
		}
		for _, f := range []agent.RunFilter{{}, {LeaseLapsed: true}, {Prefix: "t/", ExcludeHolding: []string{"run:complete"}}} {
			n := 0
			for _, err := range s.Runs(ctx, f) {
				n++
				check("Runs", err)
			}
			if n != 1 {
				t.Errorf("%s: Runs(%+v) yielded %d results, want exactly the error", name, f, n)
			}
		}
		_, err = s.AcquireLease(ctx, "r", "h", time.Minute)
		check("AcquireLease", err)
		_, err = s.RenewLease(ctx, "r", "h", time.Minute)
		check("RenewLease", err)
		check("ReleaseLease", s.ReleaseLease(ctx, "r", "h"))
		_, err = s.ReapLeases(ctx, nil)
		check("ReapLeases", err)
		_, err = s.ReapLeases(ctx, []string{"run:complete", "run:aborted"})
		check("ReapLeases(ended)", err)
		check("EventStore.Append", es.Append(ctx, "r", 0, []byte("leaf")))
		_, err = es.Load(ctx, "r")
		check("EventStore.Load", err)
	}
}

func TestPrefixEnd(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"ab", "ac", true},
		{"a\xff", "b", true},
		{"a\xff\xff", "b", true},
		{"\x00", "\x01", true},
		{"\xff", "", false},
		{"\xff\xff", "", false},
		{"", "", false},
	} {
		got, ok := prefixEnd(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("prefixEnd(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// The prefix filter admits exactly the IDs that start with it, including a prefix whose upper
// bound is the next byte value.
func TestRuns_PrefixIsExact(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"a", "ab", "abÿ", "ac", "b", "aé"} {
		if _, _, err := s.Insert(ctx, id, "n", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for prefix, want := range map[string][]string{
		"ab": {"ab", "abÿ"},
		"a":  {"a", "ab", "abÿ", "ac", "aé"},
		"b":  {"b"},
		"z":  nil,
	} {
		var got []string
		for id, err := range s.Runs(ctx, agent.RunFilter{Prefix: prefix}) {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("prefix %q = %q, want %q", prefix, got, want)
		}
	}
}

// A hole in a run's stored trail (rows 0 and 2) is reported, not read as a shorter trail.
func TestEventStore_LoadRefusesAGap(t *testing.T) {
	ctx := context.Background()
	s, err := Open(tempJournal(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, seq := range []int{0, 2} {
		if _, err := s.w.Exec(`INSERT INTO bide_events (run_id, seq, leaf) VALUES ('r', ?, x'01')`, seq); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.EventStore().Load(ctx, "r")
	if !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), "not contiguous at 1 (found 2)") {
		t.Errorf("Load = %v, want ErrStorage naming the gap at 1", err)
	}
}

func TestEventStore_AppendRules(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	es := s.EventStore()
	if err := es.Append(ctx, "r", -1, []byte("x")); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("Append(-1) = %v, want a negative-position error", err)
	}
	if err := es.Append(ctx, "r", 1, []byte("x")); err == nil || !strings.Contains(err.Error(), "non-contiguous") {
		t.Errorf("Append(past the next position) = %v, want a non-contiguous error", err)
	}
	if err := es.Append(ctx, "r", 0, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := es.Append(ctx, "r", 0, []byte("a")); err != nil {
		t.Errorf("re-appending the same leaf = %v, want a no-op", err)
	}
	if err := es.Append(ctx, "r", 0, []byte("b")); !errors.Is(err, audit.ErrSeqTaken) {
		t.Errorf("a different leaf at a stored position = %v, want audit.ErrSeqTaken", err)
	}
	got, err := es.Load(ctx, "r")
	if err != nil || len(got) != 1 || string(got[0]) != "a" {
		t.Errorf("Load = %q, %v; want the first leaf only", got, err)
	}
}
