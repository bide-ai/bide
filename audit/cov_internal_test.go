package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// An entry added under a key the cache already holds replaces the value and counts as a use, so a
// later insert evicts the other entry.
func TestKeyCache_AddReplacesAndRefreshes(t *testing.T) {
	c := newKeyCache[int](2)
	c.mu.Lock()
	c.add("a", 1)
	c.add("b", 2)
	c.add("a", 10) // a is now the most recently used
	c.add("c", 3)  // evicts b, not a
	c.mu.Unlock()
	if !c.contains("a") || c.contains("b") || !c.contains("c") || c.len() != 2 {
		t.Fatalf("a=%v b=%v c=%v len=%d, want a and c", c.contains("a"), c.contains("b"), c.contains("c"), c.len())
	}
	if got := c.get("a", func() (int, bool) { return 99, true }); got != 10 {
		t.Errorf("a = %d, want the replaced value 10", got)
	}
}

func notifyLeaf(t *testing.T, kind string, event any, salt []byte) []byte {
	t.Helper()
	inner, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(eventLeaf{Kind: kind, Event: inner, Salt: salt})
	if err != nil {
		t.Fatal(err)
	}
	return tagged(eventLeafTag, b)
}

// A stored leaf is read as a notify record only when it says it is one, and says the same thing
// about itself in its kind and in its body.
func TestDecodeNotifyLeaf(t *testing.T) {
	salt := bytes.Repeat([]byte{7}, agent.SaltSize)
	rec := agent.NotifyRecord{Kind: agent.NotifyObserved, Key: "k", RunID: "r", At: time.Unix(1, 0).UTC(), EventKind: "approval"}
	leaf, err := canonicalEvent(rec, salt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeNotifyLeaf(leaf)
	if err != nil || got.Kind != agent.NotifyObserved || got.Key != "k" || got.RunID != "r" {
		t.Fatalf("a genuine leaf = %+v, %v", got, err)
	}
	for name, tc := range map[string]struct {
		leaf []byte
		want string
	}{
		"not an event leaf":     {[]byte("garbage"), "not an event leaf"},
		"leaf body not json":    {tagged(eventLeafTag, []byte("{")), "unexpected end of JSON input"},
		"an event, not notify":  {notifyLeaf(t, "turn_started", agent.TurnStarted{}, salt), `leaf kind "turn_started" is not a notify record`},
		"record body not json":  {tagged(eventLeafTag, []byte(`{"kind":"notify_observed","event":"x","salt":""}`)), "cannot unmarshal"},
		"kind says another one": {notifyLeaf(t, "notify_acked", rec, salt), `leaf kind "notify_acked" holds a "observed" record`},
	} {
		if _, err := decodeNotifyLeaf(tc.leaf); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

// failingEventStore is an EventStore whose Append always fails and whose Load serves leaves.
type failingEventStore struct {
	leaves  [][]byte
	loadErr error
	appErr  error
}

func (f *failingEventStore) Append(context.Context, string, int, []byte) error { return f.appErr }
func (f *failingEventStore) Load(context.Context, string) ([][]byte, error) {
	return f.leaves, f.loadErr
}

// A notify trail whose append failed reloads to see whether the write landed: its own record
// already there is a no-op, another's is a conflict, nothing there is the failure itself, and a
// trail that cannot be read, or holds a leaf that is not a notify record, is reported as such.
func TestNotifyTrail_AppendFailurePaths(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("append refused")
	salt := bytes.Repeat([]byte{1}, agent.SaltSize)
	mine := agent.NotifyRecord{Kind: agent.NotifyObserved, Key: "k", RunID: "r", At: time.Unix(1, 0).UTC(), EventKind: "approval"}
	theirs := mine
	theirs.Key = "other"
	leafOf := func(r agent.NotifyRecord) []byte {
		l, err := canonicalEvent(r.Normalized(), salt)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	if err := NewNotifyTrail(&failingEventStore{appErr: boom, leaves: [][]byte{leafOf(mine)}}).Append(ctx, "t", 0, mine); err != nil {
		t.Errorf("our own record already at the position = %v, want a no-op", err)
	}
	err := NewNotifyTrail(&failingEventStore{appErr: boom, leaves: [][]byte{leafOf(theirs)}}).Append(ctx, "t", 0, mine)
	if !errors.Is(err, agent.ErrTrailConflict) || !errors.Is(err, boom) {
		t.Errorf("another record at the position = %v, want ErrTrailConflict wrapping the store's error", err)
	}
	err = NewNotifyTrail(&failingEventStore{appErr: boom}).Append(ctx, "t", 0, mine)
	if !errors.Is(err, boom) || errors.Is(err, agent.ErrTrailConflict) || !strings.Contains(err.Error(), "append at 0") {
		t.Errorf("nothing at the position = %v, want the store's error alone", err)
	}
	reload := errors.New("cannot reload")
	err = NewNotifyTrail(&failingEventStore{appErr: boom, loadErr: reload}).Append(ctx, "t", 0, mine)
	if !errors.Is(err, boom) || !errors.Is(err, reload) {
		t.Errorf("append and reload both fail = %v, want both errors", err)
	}
	err = NewNotifyTrail(&failingEventStore{appErr: boom, leaves: [][]byte{[]byte("junk")}}).Append(ctx, "t", 0, mine)
	if err == nil || !strings.Contains(err.Error(), "position 0") || !strings.Contains(err.Error(), "not an event leaf") {
		t.Errorf("a junk leaf at the position = %v", err)
	}
	if _, err := NewNotifyTrail(&failingEventStore{loadErr: reload}).Load(ctx, "t"); !errors.Is(err, reload) || !strings.Contains(err.Error(), "load notify trail t") {
		t.Errorf("Load over a failing store = %v", err)
	}
	if _, err := NewNotifyTrail(&failingEventStore{leaves: [][]byte{leafOf(mine), []byte("junk")}}).Load(ctx, "t"); err == nil || !strings.Contains(err.Error(), "record 1") {
		t.Errorf("Load with a junk leaf second = %v, want it to name record 1", err)
	}
}

// An artifact nested in a slice or map is checked for its format too, and the path names where.
func TestCheckFormats_NestedArtifacts(t *testing.T) {
	good := ProofBundle{Format: ProofFormat, STH: SignedTreeHead{Format: STHFormat}}
	bad := ProofBundle{Format: "bide.audit.proof.v0", STH: SignedTreeHead{Format: STHFormat}}
	if err := checkFormats(good); err != nil {
		t.Fatalf("a bundle of the current format: %v", err)
	}
	for name, tc := range map[string]struct {
		v    any
		path string
	}{
		"in a slice":   {[]ProofBundle{good, bad}, "[1]"},
		"in a map":     {map[string]ProofBundle{"k": bad}, "[k]"},
		"behind a ptr": {&bad, ""},
		"in a struct":  {EvidenceAction{Bundle: bad}, "Bundle"},
	} {
		err := checkFormats(tc.v)
		if !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", name, err)
			continue
		}
		if tc.path != "" && !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: err = %v, want it to name %s", name, err, tc.path)
		}
	}
	// Bytes, types of other packages, and a nil pointer hold no artifact.
	var nilBundle *ProofBundle
	if err := checkFormats(struct {
		B []byte
		T time.Time
		P *ProofBundle
	}{B: []byte("x"), T: time.Now(), P: nilBundle}); err != nil {
		t.Errorf("non-artifact fields: %v", err)
	}
}

func TestPathPrefix(t *testing.T) {
	if got := pathPrefix(""); got != "" {
		t.Errorf("pathPrefix(\"\") = %q", got)
	}
	if got := pathPrefix(".a.b"); got != "a.b " {
		t.Errorf("pathPrefix(.a.b) = %q, want %q", got, "a.b ")
	}
}
