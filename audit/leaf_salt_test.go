package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A proof bundle's audit path holds the hashes of the proven record's neighbours: path[0] is the
// adjacent record's leaf hash. If that hash is a function of the record alone, anyone holding
// the bundle confirms a low-entropy neighbour (an approval decision, {"fraud_flag":true}) by
// hashing each candidate and comparing. Every leaf therefore commits to a random salt that only
// its own record's bundle discloses.
func TestNeighbourLeafNotGuessable(t *testing.T) {
	ctx := context.Background()
	st := agent.NewMemStore()
	do := func(name string, r agent.Record) {
		t.Helper()
		if _, err := st.Do(ctx, "run", name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	do("toolu_A", agent.Record{Kind: agent.StepToolResult, ToolUseID: "toolu_A", Result: json.RawMessage(`{"charged":true}`)})
	do("toolu_B", agent.Record{Kind: agent.StepToolResult, ToolUseID: "toolu_B", Result: json.RawMessage(`{"fraud_flag":true}`)})

	_, priv, _ := ed25519.GenerateKey(nil)
	th, err := NewTreeHead(ctx, st, "run", 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProveToolCall(ctx, st, "run", "toolu_A", SignTreeHead(th, priv))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Inclusion.Path) != 1 {
		t.Fatalf("audit path has %d hashes, want 1", len(b.Inclusion.Path))
	}
	// The auditor holds only b and guesses the neighbour: its name and ID follow from the proven
	// record, its result is one of two values. Every way the auditor can hash a guess, with what
	// the bundle discloses, must miss.
	for _, guess := range []string{`{"fraud_flag":false}`, `{"fraud_flag":true}`} {
		cand := agent.Record{Name: "toolu_B", Kind: agent.StepToolResult, ToolUseID: "toolu_B", Result: json.RawMessage(guess)}
		enc, err := agent.EncodeRecord(cand)
		if err != nil {
			t.Fatal(err)
		}
		tries := [][]byte{leafHash(enc)}
		if leaf, err := canonicalRecord(cand); err == nil {
			tries = append(tries, leafHash(leaf))
		}
		sum := sha256.Sum256(append([]byte{0}, enc...))
		tries = append(tries, sum[:])
		for _, h := range tries {
			if bytes.Equal(h, b.Inclusion.Path[0]) {
				t.Errorf("the undisclosed neighbour %s was confirmed from the bundle's audit path", guess)
			}
		}
	}

	// The bundle discloses its own record's salt, which its leaf needs, and verifies; the same
	// record under any other salt does not.
	pub := priv.Public().(ed25519.PublicKey)
	if len(b.Record.Salt) != agent.SaltSize {
		t.Fatalf("the bundle's record carries a %d-byte salt, want %d", len(b.Record.Salt), agent.SaltSize)
	}
	if ok, err := b.Verify(pub); !ok || err != nil {
		t.Fatalf("bundle does not verify: %v, %v", ok, err)
	}
	b.Record.Salt = bytes.Repeat([]byte{9}, agent.SaltSize)
	if ok, _ := b.Verify(pub); ok {
		t.Fatal("a bundle verified with its record's salt changed")
	}
}

// recordsStore is a read-only Durable whose history is exactly the records it holds.
type recordsStore []agent.Record

func (s recordsStore) History(context.Context, string) ([]agent.Record, error) { return s, nil }

func (recordsStore) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("recordsStore is read-only")
}

// A record without a salt of agent.SaltSize bytes is never committed to a Merkle tree or proven:
// its leaf would be guessable from its content.
func TestUnsaltedRecordRefused(t *testing.T) {
	ctx := context.Background()
	for _, salt := range [][]byte{nil, make([]byte, agent.SaltSize-1), make([]byte, agent.SaltSize+1)} {
		st := recordsStore{{Name: "s", Kind: agent.StepValue, Result: json.RawMessage(`1`), Salt: salt}}
		if _, err := Root(ctx, st, "r"); err == nil || !strings.Contains(err.Error(), "salt") {
			t.Errorf("Root over a record with a %d-byte salt = %v, want a salt error", len(salt), err)
		}
		if _, err := NewTreeHead(ctx, st, "r", 1); err == nil {
			t.Errorf("NewTreeHead committed a record with a %d-byte salt", len(salt))
		}
		if _, err := Prove(ctx, st, "r", 0); err == nil {
			t.Errorf("Prove proved a record with a %d-byte salt", len(salt))
		}
		if ok, err := VerifyInclusion(nil, st[0], Inclusion{Size: 1}); ok || err == nil {
			t.Errorf("VerifyInclusion accepted a record with a %d-byte salt", len(salt))
		}
	}
}

// The exact bytes each kind of leaf hashes, as documented in merkle.go and mirrored by
// audit/verify: SHA-256(0x00 || tag || content).
func TestLeafFormats(t *testing.T) {
	ctx := context.Background()
	h := func(parts ...string) []byte {
		s := sha256.Sum256([]byte("\x00" + strings.Join(parts, "")))
		return s[:]
	}
	h2 := func(parts ...string) []byte {
		s := sha256.Sum256([]byte(strings.Join(parts, "")))
		return s[:]
	}

	st := agent.NewMemStore()
	if _, err := st.Do(ctx, "r", "s", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"v"`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := st.History(ctx, "r")
	enc, _ := agent.EncodeRecord(recs[0])
	want := fmt.Sprintf(`{"name":"s","kind":"value","result":"v","salt":"%s"}`, base64.StdEncoding.EncodeToString(recs[0].Salt))
	if string(enc) != want {
		t.Fatalf("journal encoding = %s, want %s", enc, want)
	}
	if root, _ := Root(ctx, st, "r"); !bytes.Equal(root, h("bide.audit.journal-leaf.v1\x00", string(enc))) {
		t.Error("a journal leaf does not hash as SHA-256(0x00 || \"bide.audit.journal-leaf.v1\\x00\" || EncodeRecord(record))")
	}

	keyRecs := recordsStore{{Name: "c", Kind: agent.StepToolResult, ToolUseID: "c", Salt: recs[0].Salt}}
	if root := AbsenceRoot(keyRecs, ToolUseKeys); !bytes.Equal(root, h("bide.audit.key-leaf.v1\x00", "tooluse:c")) {
		t.Error("a key leaf does not hash as SHA-256(0x00 || \"bide.audit.key-leaf.v1\\x00\" || key)")
	}

	log := NewEventLog()
	if err := log.Add(agent.TurnStarted{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	evProof, _ := log.Prove(0)
	evJSON := fmt.Sprintf(`{"kind":"TurnStarted","event":{"Seq":1},"salt":"%s"}`, base64.StdEncoding.EncodeToString(evProof.Salt))
	if len(evProof.Salt) != agent.SaltSize || !bytes.Equal(log.Root(), h("bide.audit.event-leaf.v2\x00", evJSON)) {
		t.Error("an event leaf does not hash as SHA-256(0x00 || \"bide.audit.event-leaf.v2\\x00\" || event JSON with its salt)")
	}

	// A projected event's salt is SHA-256("bide.audit.event-salt.v1\x00" || its record's salt), the
	// salt of the record it projects, not of a record before it that projects no event.
	toolRec := agent.Record{Name: "c", Kind: agent.StepToolResult, ToolUseID: "c", Result: json.RawMessage(`1`), Salt: recs[0].Salt}
	valueRec := agent.Record{Name: "v", Kind: agent.StepValue, Result: json.RawMessage(`2`), Salt: bytes.Repeat([]byte{5}, agent.SaltSize)}
	projected, err := EventLogFromJournal(ctx, recordsStore{valueRec, toolRec}, "r")
	if err != nil {
		t.Fatal(err)
	}
	pp, _ := projected.Prove(0)
	if !bytes.Equal(pp.Salt, h2("bide.audit.event-salt.v1\x00", string(recs[0].Salt))) {
		t.Error("a projected event's salt is not SHA-256(\"bide.audit.event-salt.v1\\x00\" || record salt)")
	}

	anchors := NewMemAnchorLog()
	if err := anchors.Publish(ctx, "r", SignedTreeHead{TreeHead: TreeHead{Kind: TreeJournal, RunID: "r"}}); err != nil {
		t.Fatal(err)
	}
	entry, _ := json.Marshal(anchors.Entries()[0])
	if root, _ := anchors.Root(); !bytes.Equal(root, h("bide.audit.anchor-leaf.v1\x00", string(entry))) {
		t.Error("an anchor leaf does not hash as SHA-256(0x00 || \"bide.audit.anchor-leaf.v1\\x00\" || entry JSON)")
	}
}

// An event-log proof's audit path holds the hashes of the proven event's neighbours, as a journal
// bundle's does: path[0] is the adjacent event's leaf hash. Events carry content (a tool's result,
// an assistant turn), so if that hash is a function of the event alone, anyone holding the proof
// confirms a low-entropy neighbour by hashing each candidate and comparing. Every event leaf
// therefore commits to a random salt that only its own event's proof discloses. This holds for a
// log filled live, for the journal projection, and for a log rebuilt from an EventStore.
func TestNeighbourEventNotGuessable(t *testing.T) {
	ctx := context.Background()
	result := func(id, res string) agent.ToolCompleted {
		return agent.ToolCompleted{ToolUseID: id, Result: json.RawMessage(res)}
	}

	live := NewEventLog()
	for _, e := range []agent.AgentEvent{result("toolu_A", `{"charged":true}`), result("toolu_B", `{"fraud_flag":true}`)} {
		if err := live.Add(e); err != nil {
			t.Fatal(err)
		}
	}

	st := agent.NewMemStore()
	for _, r := range []agent.Record{
		{Kind: agent.StepToolResult, ToolUseID: "toolu_A", Result: json.RawMessage(`{"charged":true}`)},
		{Kind: agent.StepToolResult, ToolUseID: "toolu_B", Result: json.RawMessage(`{"fraud_flag":true}`)},
	} {
		if _, err := st.Do(ctx, "run", r.ToolUseID, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	projected, err := EventLogFromJournal(ctx, st, "run")
	if err != nil {
		t.Fatal(err)
	}
	evStore := NewMemEventStore()
	if err := PersistJournal(ctx, evStore, st, "run"); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadEventLog(ctx, evStore, "run")
	if err != nil {
		t.Fatal(err)
	}

	for name, log := range map[string]*EventLog{"live": live, "journal projection": projected, "event store": loaded} {
		proof, err := log.Prove(0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(proof.Path) != 1 {
			t.Fatalf("%s: audit path has %d hashes, want 1", name, len(proof.Path))
		}
		// The auditor holds the proof and guesses the next event: its ID follows from the proven
		// call, its result is one of two values. Every way the auditor can hash a guess, with what
		// the proof discloses, must miss.
		for _, guess := range []string{`{"fraud_flag":false}`, `{"fraud_flag":true}`} {
			cand := result("toolu_B", guess)
			inner, _ := json.Marshal(cand)
			body, _ := json.Marshal(struct {
				Kind  string          `json:"kind"`
				Event json.RawMessage `json:"event"`
			}{"ToolCompleted", inner})
			tries := [][]byte{leafHash(body), leafHash(append([]byte("bide.audit.event-leaf.v1\x00"), body...)), leafHash(tagged(eventLeafTag, body))}
			if leaf, err := canonicalEvent(cand, proof.Salt); err == nil { // the one salt the proof discloses
				tries = append(tries, leafHash(leaf))
			}
			for _, h := range tries {
				if bytes.Equal(h, proof.Path[0]) {
					t.Errorf("%s: the undisclosed neighbour %s was confirmed from the proof's audit path", name, guess)
				}
			}
		}
	}
}
