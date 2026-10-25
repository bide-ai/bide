package runview

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// The four flags are registered with the shared help text and fill the struct; bide view and bide edit
// register them through the same code.
func TestStoreFlagsRegister(t *testing.T) {
	var f StoreFlags
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.Register(fs)
	if err := fs.Parse([]string{"-db", "a.db", "-postgres", "dsn", "-postgres-env", "VAR", "-journal", "j1", "-journal", "j2"}); err != nil {
		t.Fatal(err)
	}
	if f.DB != "a.db" || f.Postgres != "dsn" || f.PostgresEnv != "VAR" || len(f.Journals) != 2 || f.Journals[1] != "j2" {
		t.Errorf("flags %+v", f)
	}
	for name, want := range map[string]string{"db": DBFlagHelp, "postgres": PostgresFlagHelp, "postgres-env": PostgresEnvFlagHelp, "journal": JournalFlagHelp} {
		if got := fs.Lookup(name).Usage; got != want {
			t.Errorf("-%s help %q", name, got)
		}
	}
}

func TestStoreFlagsKindsAndValidate(t *testing.T) {
	for name, c := range map[string]struct {
		f     StoreFlags
		kinds int
		msg   string
	}{
		"none":            {StoreFlags{}, 0, ""},
		"db":              {StoreFlags{DB: "a"}, 1, ""},
		"postgres":        {StoreFlags{Postgres: "p"}, 1, ""},
		"postgres env":    {StoreFlags{PostgresEnv: "ORDERS_DSN"}, 1, ""},
		"journals":        {StoreFlags{Journals: []string{"a", "b"}}, 1, ""},
		"db and journal":  {StoreFlags{DB: "a", Journals: []string{"b"}}, 2, "give exactly one of"},
		"both postgres":   {StoreFlags{Postgres: "p", PostgresEnv: "V"}, 1, "give -postgres or -postgres-env, not both"},
		"env is a dsn":    {StoreFlags{PostgresEnv: "postgres://u:secret@h/db"}, 1, "environment variable"},
		"db and postgres": {StoreFlags{DB: "a", Postgres: "p"}, 2, "give exactly one of"},
	} {
		if got := c.f.Kinds(); got != c.kinds {
			t.Errorf("%s: kinds %d", name, got)
		}
		msg := c.f.Validate()
		if (c.msg == "") != (msg == "") || !strings.Contains(msg, c.msg) {
			t.Errorf("%s: message %q, want %q", name, msg, c.msg)
		}
		if strings.Contains(msg, "secret") {
			t.Errorf("%s: the message echoes the value: %q", name, msg)
		}
	}
}

func TestStoreFlagsOpen(t *testing.T) {
	// No flags opens nothing.
	if o, err := (StoreFlags{}).Open(Stores{}); err != nil || o.Source != nil || o.Closer != nil {
		t.Errorf("none: %+v %v", o, err)
	}
	// A variable that is not set is named, its value never.
	t.Setenv("BIDE_TEST_EMPTY_DSN", "")
	if _, err := (StoreFlags{PostgresEnv: "BIDE_TEST_EMPTY_DSN"}).Open(Stores{}); err == nil || !strings.Contains(err.Error(), "BIDE_TEST_EMPTY_DSN: the variable is empty or not set") {
		t.Errorf("empty env: %v", err)
	}
	// A build without the store says so.
	if _, err := (StoreFlags{DB: "x"}).Open(Stores{}); err == nil || !strings.Contains(err.Error(), "this build cannot read that store") {
		t.Errorf("no opener: %v", err)
	}
	// Exported journals are read whole.
	in := newCLIInputs(t)
	o, err := (StoreFlags{Journals: []string{in.journal}}).Open(Stores{})
	if err != nil || o.Journal != nil || o.Store != nil || o.Closer != nil {
		t.Fatalf("journal: %+v %v", o, err)
	}
	if _, ok := o.Source.(Exports); !ok {
		t.Errorf("source %T", o.Source)
	}
}
