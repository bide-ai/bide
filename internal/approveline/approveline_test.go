package approveline

import "testing"

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		j                        Journal
		run, call, approver, key string
		single                   bool
		want                     string
	}{
		{"sqlite", Journal{SQLite: "run.db"}, "refund-1049", "c2", "ops", "ops.key", false,
			"bide approve -db run.db -run refund-1049 -call c2 -as ops -key ops.key"},
		{"placeholders", Journal{SQLite: "run.db"}, "r", "c", "", "", false,
			`bide approve -db run.db -run r -call c -as '<approver>' -key '<key-file>'`},
		{"postgres names the variable, never a DSN", Journal{PostgresEnv: "BIDE_DSN"}, "r", "c", "ops", "k", false,
			"bide approve -postgres-env BIDE_DSN -run r -call c -as ops -key k"},
		{"single gate has no approver or key", Journal{SQLite: "run.db"}, "r", "node:review", "ops", "k", true,
			"bide approve -db run.db -run r -call node:review"},
		{"quoting stops a hostile id from splitting or running", Journal{SQLite: "my run.db"}, "r'; rm -rf ~ #", "c$(id)", "ops", "k", false,
			`bide approve -db 'my run.db' -run 'r'\''; rm -rf ~ #' -call 'c$(id)' -as ops -key k`},
		{"empty value is an empty word, not a vanished one", Journal{SQLite: "run.db"}, "", "c", "ops", "k", false,
			"bide approve -db run.db -run '' -call c -as ops -key k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Command(tc.j, tc.run, tc.call, tc.approver, tc.key, tc.single); got != tc.want {
				t.Fatalf("got\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}
