package audit_test

// Focused review of the #105 fixes after the re-review (head 7854337). Each Test_R105c_* asserts a
// property the fixes or their docs claim; a failing test is a finding.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// r105cJournal journals, in run "r", a model turn that requested call "x" and the call's result.
func r105cJournal(t *testing.T) (*agent.MemStore, []agent.Record) {
	t.Helper()
	ctx := context.Background()
	s := agent.NewMemStore()
	turn := agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "x", Name: "wire", Args: json.RawMessage(`{}`)}}}
	if _, err := s.Do(ctx, "r", "@llm/0", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepModel, Message: &turn}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "r", agent.ToolResultStep("x"), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "x", Result: json.RawMessage(`"sent"`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, err := s.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	return s, recs
}

// The projection reads a record's decoded fields, while the journal root that journalPrefix
// checks binds only its stored bytes (Raw). Records whose fields no longer say what their bytes
// say (changed in memory after History; Raw is unexported, so it survives a struct copy) pass the
// journal-root check and checkRecordBytes, and the key set is projected from the changed fields.
// NewAbsenceTreeHead then signs a head without "tooluse:x", ProveAbsentBundle proves the call that
// ran absent, the bundle verifies, and AbsenceRoot over the same records "confirms" the head. The
// docs claim the head commits to "the key set projected from exactly those records" and that
// absence holds for "the real history up to the committed journal size, not merely from a set the
// prover chose".
func Test_R105c_ProjectionReadsFieldsTheJournalRootDoesNotBind(t *testing.T) {
	ctx := context.Background()
	s, recs := r105cJournal(t)
	changed := slices.Clone(recs)
	// changed[0] is the journal header; [1] the model turn; [2] the call's result.
	changed[1].Message = &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "no calls"}}}
	changed[2].Kind = agent.StepValue
	if len(changed[2].Raw()) == 0 {
		t.Fatal("sanity: the changed copy lost its stored bytes")
	}

	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	jth, err := audit.NewTreeHead(ctx, s, "r", p11Now())
	if err != nil {
		t.Fatal(err)
	}
	abs, err := audit.SignAbsenceRoot(changed, audit.ToolUseKeys, jth, signer, p11Now())
	if err != nil {
		t.Logf("SignAbsenceRoot refused: %v (good)", err)
		return
	}
	b, err := audit.ProveAbsentBundle(changed, audit.ToolUseKeys, audit.ToolUseKeyFor("x"), abs)
	if err != nil {
		t.Logf("ProveAbsentBundle refused: %v (good)", err)
		return
	}
	if err := b.Verify(v, audit.ToolUseKeys); err != nil {
		t.Logf("bundle does not verify: %v (good)", err)
		return
	}
	root, size, err := audit.AbsenceRoot(changed, audit.ToolUseKeys)
	t.Logf("AbsenceRoot over the same records: size %d, matches the signed head %v, err %v", size, string(root) == string(abs.Root) && size == abs.Size, err)
	t.Fatalf("call x ran (journal size %d, head bound to it) and was proven absent: the key set was projected from fields the journal root does not bind", jth.Size)
}

// checkFormats on Go values with nil pointers, empty slices and zero nested artifacts must not
// panic, and must report ErrFormat for a nested head of another format.
func Test_R105c_CheckFormatsZeroAndNilValues(t *testing.T) {
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	for name, f := range map[string]func(){
		"zero cert":      func() { _, _ = audit.VerifyRun(audit.RunCertificate{}, nil, v) },
		"zero package":   func() { _, _ = audit.EvidencePackage{}.Verify(v) },
		"nil pointers":   func() { _, _ = audit.EvidencePackage{Format: audit.EvidenceFormat, Grants: nil, RunCertificate: nil, Consistency: nil}.Verify(v) },
		"empty grants":   func() { _, _ = audit.EvidencePackage{Format: audit.EvidenceFormat, Grants: &audit.EvidenceGrants{}}.Verify(v) },
		"zero anchor":    func() { _ = audit.VerifyAnchorInclusion(nil, audit.AnchorEntry{}, audit.Inclusion{}) },
		"cert in pkg":    func() { _, _ = audit.EvidencePackage{Format: audit.EvidenceFormat, RunCertificate: &audit.RunCertificate{}}.Verify(v) },
		"conv zero cert": func() { _, _ = audit.VerifyRun(audit.RunCertificate{Format: audit.RunCertificateFormat, Convergence: make([]audit.PolicyConvergence, 3)}, nil, v) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			f()
		})
	}
}

// The same finding through each projection that takes records: a record whose decoded fields no
// longer say what its stored bytes say is refused with ErrMalformed, never projected from the
// changed fields.
func Test_R105c_EveryProjectionRefusesFieldsThatDisagreeWithTheStoredBytes(t *testing.T) {
	ctx := context.Background()
	s, recs := r105cJournal(t)
	changed := slices.Clone(recs)
	changed[2].Kind = agent.StepValue
	jth, err := audit.NewTreeHead(ctx, s, "r", p11Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []audit.KeySet{audit.ToolUseKeys, audit.PolicyUsedKeys} {
		if _, err := audit.NewAbsenceTreeHead(changed, set, jth, p11Now()); !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("NewAbsenceTreeHead(%s): err %v, want ErrMalformed", set.Kind, err)
		}
		if _, err := audit.ProveAbsent(changed, set, set.Prefix+"y"); !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("ProveAbsent(%s): err %v, want ErrMalformed", set.Kind, err)
		}
		if _, _, err := audit.AbsenceRoot(changed, set); !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("AbsenceRoot(%s): err %v, want ErrMalformed", set.Kind, err)
		}
	}
	if _, err := audit.PoliciesUsed(changed); !errors.Is(err, audit.ErrMalformed) {
		t.Errorf("PoliciesUsed: err %v, want ErrMalformed", err)
	}
	// Unchanged records still project.
	if _, _, err := audit.AbsenceRoot(recs, audit.ToolUseKeys); err != nil {
		t.Errorf("AbsenceRoot over the records as read: %v", err)
	}
}
