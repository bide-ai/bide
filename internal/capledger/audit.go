package capledger

import (
	"fmt"
	"strings"
)

// What bide audit caps reads of the ledger: the name and the decoding of the audit record a reservation
// leaves in its call's run (cap-reserve), and the encoding of a free-text segment of a ledger's run ID.
// The audit never writes; these are the same functions the writer uses, so the two cannot drift.

// Reserve is the decoded cap-reserve record of a reservation: the ledger run it was written into, the
// slot's number and the window's total after it.
type Reserve struct {
	Ledger string
	Slot   int
	Cum    int64
}

// IsReserveName reports whether an entry name is that of a cap-reserve record.
func IsReserveName(name string) bool { return strings.HasPrefix(name, reservePrefix) }

// ReserveName is the name of the cap-reserve record of attempt (an AttemptKey) in its call's run.
func (l *Ledger) ReserveName(attempt string) string { return reservePrefix + l.cfg.Encode(attempt) }

// Segment is s as the ledger's configured Encode writes it into a run ID, so a caller can match a
// namespace, a rule or a window against the segments of a ledger run.
func (l *Ledger) Segment(s string) string { return l.cfg.Encode(s) }

// DecodeReserve decodes the data of a cap-reserve record, strictly: a member it does not know, another
// format or data after the record is an error.
func DecodeReserve(b []byte) (Reserve, error) {
	var r reserveRecord
	if err := strict(b, &r); err != nil {
		return Reserve{}, fmt.Errorf("cap-reserve: %w", err)
	}
	if r.Format != reserveFormat {
		return Reserve{}, fmt.Errorf("cap-reserve: format %q, want %q", r.Format, reserveFormat)
	}
	return Reserve{Ledger: r.Ledger, Slot: r.Slot, Cum: r.Cum}, nil
}

// LedgerSegments splits a ledger run ID into its namespace, rule, currency, window and key-digest
// segments (each as encoded in the ID). ok is false for anything but a ledger run (an index run has
// four segments, not five).
func LedgerSegments(run string) (ns, rule, currency, window, key string, ok bool) {
	seg := strings.Split(run, "/")
	if len(seg) != 6 || seg[0]+"/" != RunPrefix {
		return "", "", "", "", "", false
	}
	return seg[1], seg[2], seg[3], seg[4], seg[5], true
}

// Intent is the decoded cap-intent record of an attempt: what it reserves, and the window fixed for it
// once. Floor is the newest window the index held when the clock bound was checked ("" for a run window
// or an empty index).
type Intent struct {
	Attempt   string
	Namespace string
	Rule      string
	Currency  string
	Kind      Kind
	Key       string
	Amount    int64
	Marked    bool
	Window    string
	Floor     string
}

// IsIntentName reports whether an entry name is that of a cap-intent record.
func IsIntentName(name string) bool { return strings.HasPrefix(name, intentPrefix) }

// IntentName is the name of the cap-intent record of attempt (an AttemptKey) in its call's run.
func (l *Ledger) IntentName(attempt string) string { return intentPrefix + l.cfg.Encode(attempt) }

// DecodeIntent decodes the data of a cap-intent record, strictly.
func DecodeIntent(b []byte) (Intent, error) {
	in, err := decodeIntent(b)
	if err != nil {
		return Intent{}, err
	}
	return Intent{Attempt: in.Attempt, Namespace: in.Namespace, Rule: in.Rule, Currency: in.Currency, Kind: in.Kind,
		Key: in.Key, Amount: in.Amount, Marked: in.Marked, Window: in.Window, Floor: in.Floor}, nil
}

// IntentRule is the rule an intent names.
func (in Intent) IntentRule() Rule {
	return Rule{Namespace: in.Namespace, Name: in.Rule, Currency: in.Currency, Kind: in.Kind}
}

// IntentLedger is the ledger run an intent sends its attempt's slot to.
func (l *Ledger) IntentLedger(in Intent) string {
	return l.LedgerRun(in.IntentRule(), in.Window, in.Key)
}
