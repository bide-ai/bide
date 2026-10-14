package approveline

import "testing"

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		j                            Journal
		run, call, approver, key, ks string
		single                       bool
		want                         string
	}{
		{"sqlite", Journal{SQLite: "run.db"}, "refund-1049", "c2", "ops", "ops.key", "keys.json", false,
			"bide approve -db run.db -run refund-1049 -call c2 -as ops -key ops.key -approver-keys keys.json"},
		{"placeholders", Journal{SQLite: "run.db"}, "r", "c", "", "", "", false,
			`bide approve -db run.db -run r -call c -as '<approver>' -key '<key-file>' -approver-keys '<keys-file>'`},
		{"postgres names the variable, never a DSN", Journal{PostgresEnv: "BIDE_DSN"}, "r", "c", "ops", "k", "ks", false,
			"bide approve -postgres-env BIDE_DSN -run r -call c -as ops -key k -approver-keys ks"},
		{"single gate has no approver, key or keys", Journal{SQLite: "run.db"}, "r", "node:review", "ops", "k", "ks", true,
			"bide approve -db run.db -run r -call node:review"},
		{"quoting stops a hostile id from splitting or running", Journal{SQLite: "my run.db"}, "r'; rm -rf ~ #", "c$(id)", "ops", "k", "ks", false,
			`bide approve -db 'my run.db' -run 'r'\''; rm -rf ~ #' -call 'c$(id)' -as ops -key k -approver-keys ks`},
		{"empty value is an empty word, not a vanished one", Journal{SQLite: "run.db"}, "", "c", "ops", "k", "ks", false,
			"bide approve -db run.db -run '' -call c -as ops -key k -approver-keys ks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Command(tc.j, tc.run, tc.call, tc.approver, tc.key, tc.ks, tc.single); got != tc.want {
				t.Fatalf("got\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}
