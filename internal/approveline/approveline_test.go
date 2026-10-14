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
		{"a leading = is quoted (zsh expands =word to a path)", Journal{SQLite: "=ls"}, "=r", "c", "ops", "k", "ks", false,
			"bide approve -db '=ls' -run '=r' -call c -as ops -key k -approver-keys ks"},
		{"empty value is an empty word, not a vanished one", Journal{SQLite: "run.db"}, "", "c", "ops", "k", "ks", false,
			"bide approve -db run.db -run '' -call c -as ops -key k -approver-keys ks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Command(tc.j, tc.run, tc.call, tc.approver, tc.key, tc.ks, tc.single)
			if err != nil || got != tc.want {
				t.Fatalf("got\n  %s (%v)\nwant\n  %s", got, err, tc.want)
			}
		})
	}
}

// TestCommandRefusesUnsafeIDs: a run, call or approver id with a control, bidi, invisible or filler
// character, or bytes that are not UTF-8, cannot be written into a line a person reads and pastes.
func TestCommandRefusesUnsafeIDs(t *testing.T) {
	for _, r := range []rune{0x1b, 0x07, 0x7f, 0x9b, 0x202e, 0x2066, 0x200b, 0x2028, 0x3164, 0x2800, 0x115f, 0x1160, 0xffa0, 0x00ad, 0x180e, 0xfeff, 0x034f, 0xfe0f, 0xe0100, 0x17b4, 0x17b5, 0xfffc, 0x3000, 0x00a0, 0x2003, 0x0301, 0x20dd, 0x1d173} {
		bad := "c" + string(r) + "x"
		for i, args := range [][3]string{{bad, "c", "ops"}, {"r", bad, "ops"}, {"r", "c", bad}} {
			if got, err := Command(Journal{SQLite: "j.db"}, args[0], args[1], args[2], "k", "ks", false); err == nil {
				t.Errorf("U+%04X in argument %d: accepted: %s", r, i, got)
			}
		}
	}
	if _, err := Command(Journal{SQLite: "j.db"}, "r\xff", "c", "ops", "k", "ks", false); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if _, err := Command(Journal{SQLite: "j\x1b.db"}, "r", "c", "ops", "k", "ks", false); err == nil {
		t.Error("a control character in the journal path accepted")
	}
	// Ordinary non-ASCII text is fine.
	if _, err := Command(Journal{SQLite: "j.db"}, "заказ-1", "c", "ops", "k", "ks", false); err != nil {
		t.Errorf("ordinary text refused: %v", err)
	}
	if _, err := Command(Journal{SQLite: "j.db"}, "caf\u00e9", "c", "ops", "k", "ks", false); err != nil {
		t.Errorf("a precomposed accent refused: %v", err)
	}
	if !Unsafe(0x3164) || Unsafe('a') || Unsafe(' ') || Unsafe('\u00e9') {
		t.Error("Unsafe misclassifies")
	}
}
