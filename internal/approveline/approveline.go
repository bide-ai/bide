// Package approveline builds the bide approve command line for one call that waits for a decision:
// the line bide approve -list prints, and the one a notification can carry, so an approver who
// reads that a call is waiting can run it as it stands. It is the standard library only, in the
// core module's internal tree, so the notifier and the bide command build the same line.
//
// The line never carries a secret. A SQLite journal is named by its path; a Postgres journal by
// the environment variable that holds its DSN (-postgres-env), since a DSN holds a password that
// would otherwise land in a notification, a terminal scrollback and the shell history. The
// approver's private key is named by its file, never by its content.
package approveline

import (
	"strings"
)

// Journal names the journal a command opens: exactly one of SQLite and PostgresEnv.
type Journal struct {
	SQLite      string // path of the SQLite journal file (-db)
	PostgresEnv string // name of the environment variable that holds the Postgres DSN (-postgres-env)
}

// Placeholders stand for the parts only the approver knows. A line that carries one is not yet
// runnable: the approver replaces it (and the angle brackets) with their own value.
const (
	ApproverPlaceholder = "<approver>"
	KeyFilePlaceholder  = "<key-file>"
	KeysFilePlaceholder = "<keys-file>"
)

// Command returns the bide approve command line that records a decision on call callID of run
// runID in journal j. approver, keyFile and keysFile are the approver's id, private key file and the
// public-keys file bide approve checks the key against (-approver-keys, required for an m-of-n
// gate); an empty one is written as a placeholder (ApproverPlaceholder, KeyFilePlaceholder,
// KeysFilePlaceholder). A single-approval
// gate has no approver identity and no signature: pass single, and none of the three is written.
//
// Arguments are quoted for a POSIX shell, where a value needs it, so an id with a space or a
// quote (a run ID may be any string) cannot split into two arguments or run a command. A
// placeholder is quoted too, so a pasted line fails loudly instead of running something else.
func Command(j Journal, runID, callID, approver, keyFile, keysFile string, single bool) string {
	args := []string{"bide", "approve"}
	switch {
	case j.SQLite != "":
		args = append(args, "-db", quote(j.SQLite))
	case j.PostgresEnv != "":
		args = append(args, "-postgres-env", quote(j.PostgresEnv))
	}
	args = append(args, "-run", quote(runID), "-call", quote(callID))
	if !single {
		if approver == "" {
			approver = ApproverPlaceholder
		}
		if keyFile == "" {
			keyFile = KeyFilePlaceholder
		}
		if keysFile == "" {
			keysFile = KeysFilePlaceholder
		}
		args = append(args, "-as", quote(approver), "-key", quote(keyFile), "-approver-keys", quote(keysFile))
	}
	return strings.Join(args, " ")
}

// quote returns s as one POSIX shell word: unchanged when it holds only characters that need no
// quoting, else in single quotes with each embedded single quote written as '\”.
func quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
