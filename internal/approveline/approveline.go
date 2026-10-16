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
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrUnsafe reports an id that holds a character that cannot be shown or pasted safely.
var ErrUnsafe = errors.New("approveline: an id holds a control, bidirectional, invisible or filler character, or bytes that are not UTF-8")

// Unsafe reports whether r is a character that renders as nothing, or changes how the text around
// it is shown or run: a control character, a Unicode format character (bidirectional overrides and
// isolates, zero-width characters, the soft hyphen), a line or paragraph separator, any space other
// than the ASCII space (no-break, ideographic, em and en spaces), any combining or enclosing mark
// (variation selectors, the combining grapheme joiner, the Khmer inherent vowels), the object
// replacement character, or one of the fillers that render as blank (Hangul and braille fillers).
// Combining marks are refused so that an id cannot carry marks that draw nothing or restyle the
// character before them; text with accents is written precomposed (NFC), where it is ordinary. The command line and the viewer escape
// them; a line that would carry one is not written.
func Unsafe(r rune) bool {
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x2800, 0x180E, 0x2028, 0x2029, 0xFFFC:
		return true
	case ' ':
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) ||
		unicode.Is(unicode.Zs, r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r)
}

// checkText returns ErrUnsafe if s holds an unsafe character or invalid UTF-8.
func checkText(s string) error {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 || Unsafe(r) {
			return ErrUnsafe
		}
		i += n
	}
	return nil
}

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
	// CertFilePlaceholder and PolicyFilePlaceholder stand for a certificate-bound approver's chain
	// file and the identity policy file (CertCommand).
	CertFilePlaceholder   = "<chain.pem>"
	PolicyFilePlaceholder = "<identity-policy.json>"
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
func Command(j Journal, runID, callID, approver, keyFile, keysFile string, single bool) (string, error) {
	for _, s := range []string{j.SQLite, j.PostgresEnv, runID, callID, approver, keyFile, keysFile} {
		if err := checkText(s); err != nil {
			return "", err
		}
	}
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
	return strings.Join(args, " "), nil
}

// CertCommand returns the bide approve command line that records a certificate-bound decision on call
// callID of run runID in journal j: -cert names the approver's chain and -identity-policy the policy file
// the chain is checked against before anything is signed. approver is the approver's id (empty:
// ApproverPlaceholder); the key, chain and policy files are always placeholders. Quoting is
// Command's. A gate with a certificate-bound approver never takes the bare-key form of Command for
// that approver: the decision would be recorded and not counted.
func CertCommand(j Journal, runID, callID, approver string) (string, error) {
	for _, s := range []string{j.SQLite, j.PostgresEnv, runID, callID, approver} {
		if err := checkText(s); err != nil {
			return "", err
		}
	}
	if approver == "" {
		approver = ApproverPlaceholder
	}
	args := []string{"bide", "approve"}
	switch {
	case j.SQLite != "":
		args = append(args, "-db", quote(j.SQLite))
	case j.PostgresEnv != "":
		args = append(args, "-postgres-env", quote(j.PostgresEnv))
	}
	args = append(args, "-run", quote(runID), "-call", quote(callID), "-as", quote(approver),
		"-key", quote(KeyFilePlaceholder), "-cert", quote(CertFilePlaceholder), "-identity-policy", quote(PolicyFilePlaceholder))
	return strings.Join(args, " "), nil
}

// quote returns s as one POSIX shell word: unchanged when it holds only characters that need no
// quoting, else in single quotes with each embedded single quote written as '\”.
func quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := s[0] != '=' // zsh expands a word that starts with = to a command's path
	for i := 0; i < len(s) && safe; i++ {
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

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether s can be the name of an environment variable: letters, digits and
// underscores, not starting with a digit. A command that takes -postgres-env checks it before
// anything else, so that a DSN given by mistake (it holds a password) is refused without being
// echoed in an error.
func ValidEnvName(s string) bool { return envName.MatchString(s) }

// NotAnEnvName is the usage error for a -postgres-env value that is not a variable name. It never
// holds the value.
const NotAnEnvName = "-postgres-env takes the name of an environment variable, not a DSN"
