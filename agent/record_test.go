package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"
)

// legacyRecord is Record as it was before the model-turn metadata (ModelTurn) and a signed
// decision's fields (ApproverSignature) moved behind pointers: every field flat, in the order the
// journal encoding writes them. Its layout is the reference the journal encoding keeps: a record
// encodes to the same bytes as its legacyRecord, and bytes either wrote decode to the same content.
// testdata/record_identity_legacy.jsonl holds records the pre-move code encoded (see
// TestRecordIdentity_LegacyCorpus), which ties this type to that code.
type legacyRecord struct {
	Name           string          `json:"name"`
	Kind           StepKind        `json:"kind"`
	Message        *Message        `json:"message,omitempty"`
	Usage          *Usage          `json:"usage,omitempty"`
	DiscardedUsage *Usage          `json:"discarded_usage,omitempty"`
	Finish         FinishReason    `json:"finish,omitempty"`
	RawFinish      string          `json:"raw_finish,omitempty"`
	Model          *ModelInfo      `json:"model,omitempty"`
	PromptDigest   string          `json:"prompt_digest,omitempty"`
	ToolsDigest    string          `json:"tools_digest,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	IsError        bool            `json:"is_error,omitempty"`
	Approved       bool            `json:"approved,omitempty"`
	Approver       string          `json:"approver,omitempty"`
	ApproverAlg    Alg             `json:"approver_alg,omitempty"`
	Signature      []byte          `json:"signature,omitempty"`
	DecidedAt      string          `json:"decided_at,omitempty"`
	RecordedAt     string          `json:"recorded_at,omitempty"`
	ApproverCert   [][]byte        `json:"approver_cert,omitempty"`
	AttemptedAt    int64           `json:"attempted_at,omitempty"`
	Reconciled     bool            `json:"reconciled,omitempty"`
	OutcomeUnknown bool            `json:"outcome_unknown,omitempty"`
	Redacted       bool            `json:"-"`
	stamped        bool
	Safety         *Safety         `json:"safety,omitempty"`
	Approval       *ApprovalPolicy `json:"approval,omitempty"`
	Evidence       json.RawMessage `json:"evidence,omitempty"`
	Format         string          `json:"format,omitempty"`
	claim          string
	salt           []byte
	raw            []byte
}

// legacyWire and legacyReadWire are recordWire and recordReadWire over legacyRecord.
type legacyWire struct {
	*legacyRecord
	Claim string `json:"claim,omitempty"`
	Salt  []byte `json:"salt,omitempty"`
}

type legacyReadWire struct {
	legacyWire
	Approval *storedApproval `json:"approval,omitempty"`
}

func legacyMarshal(r legacyRecord) ([]byte, error) {
	return marshalJournal(legacyWire{legacyRecord: &r, Claim: r.claim, Salt: r.salt})
}

func legacyUnmarshal(b []byte) (legacyRecord, error) {
	var f legacyRecord
	w := legacyReadWire{legacyWire: legacyWire{legacyRecord: &f}}
	if err := json.Unmarshal(b, &w); err != nil {
		return legacyRecord{}, err
	}
	f.Approval = w.Approval.policy()
	f.claim, f.salt, f.raw = w.Claim, w.Salt, nil
	return f, nil
}

// legacyEncode is EncodeRecord as it was, over legacyRecord.
func legacyEncode(r legacyRecord) ([]byte, error) {
	b, err := legacyMarshal(r)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(b, []byte(jsonEscape+"fffd")) {
		return b, nil
	}
	back, err := legacyUnmarshal(b)
	if err != nil {
		return nil, err
	}
	return legacyMarshal(back)
}

// legacyView is r as a legacyRecord, read through Record's accessors, so comparing it with a
// legacyRecord checks the accessors as well as the fields that stayed in place.
func legacyView(r Record) legacyRecord {
	return legacyRecord{
		Name: r.Name, Kind: r.Kind, Message: r.Message, Usage: r.Usage, DiscardedUsage: r.DiscardedUsage,
		Finish: r.Finish(), RawFinish: r.RawFinish(), Model: r.Model(), PromptDigest: r.PromptDigest(), ToolsDigest: r.ToolsDigest(),
		ToolUseID: r.ToolUseID, Result: r.Result, IsError: r.IsError, Approved: r.Approved,
		Approver: r.Approver(), ApproverAlg: r.ApproverAlg(), Signature: r.Signature(), DecidedAt: r.DecidedAt(), RecordedAt: r.RecordedAt(), ApproverCert: r.ApproverCert(),
		AttemptedAt: r.AttemptedAt, Reconciled: r.Reconciled, OutcomeUnknown: r.OutcomeUnknown, Redacted: r.Redacted, stamped: r.stamped,
		Safety: r.Safety, Approval: r.Approval, Evidence: r.Evidence, Format: r.Format, claim: r.claim, salt: r.salt, raw: r.raw,
	}
}

// The bits of an identity mask: one per field of legacyRecord (set to its value or left zero), then
// two that give the record a ModelTurn or ApproverSignature even when none of its fields is set.
const (
	idName = iota
	idKind
	idMessage
	idUsage
	idDiscarded
	idFinish
	idRawFinish
	idModel
	idPromptDigest
	idToolsDigest
	idToolUseID
	idResult
	idIsError
	idApproved
	idApprover
	idApproverAlg
	idSignature
	idAttemptedAt
	idReconciled
	idOutcomeUnknown
	idRedacted
	idStamped
	idSafety
	idApproval
	idEvidence
	idFormat
	idClaim
	idSalt
	// A signed decision's times come last, after the fields the legacy corpus was generated with,
	// so the corpus's masks keep their meaning.
	idDecidedAt
	idRecordedAt
	idFields    // the number of fields
	idForceTurn = idFields
	idForceSig  = idFields + 1
	idBits      = idFields + 2
	idAll       = ^uint32(0) >> (32 - idBits) // every bit of a mask
)

// idVals is the content a mask's set fields take.
type idVals struct {
	s      [6]string // string fields cycle through these
	raw    json.RawMessage
	bin    []byte
	n      int64
	single bool // the Approval is SingleApproval rather than an m-of-n policy
}

// trickyVals are values that exercise every special case of the journal encoding: HTML
// characters (never escaped), U+2028 and U+2029 (always escaped), invalid UTF-8 (U+FFFD, which
// takes EncodeRecord's second pass), an escaped U+FFFD already in the text, and raw JSON with
// insignificant whitespace (compacted).
var trickyVals = []idVals{
	{s: [6]string{"a", "model", "stop", "end_turn", "sha256:p", "sha256:t"}, raw: json.RawMessage(`{"ok":true}`), bin: []byte{1, 2, 3}, n: 1_700_000_000_000, single: true},
	{s: [6]string{"<b>&amp;</b>", "x\u2028y\u2029z", "bad\xffutf8", `q"uote\`, "\ufffd", "é"}, raw: json.RawMessage(" [ 1.50 , 1e20 , \"<b>\" , \"\\u2028\" ] "), bin: []byte{0, 255}, n: -1},
	{s: [6]string{"\xfe\xff", "\\ufffd", "\u2028", "tab\tnl\n", "z", "日本"}, raw: json.RawMessage(`"\ud800"`), bin: bytes.Repeat([]byte{7}, SaltSize), n: 1 << 62, single: true},
}

// idPair builds the Record and the legacyRecord a mask and values describe.
func idPair(mask uint32, v idVals) (Record, legacyRecord) {
	l := idLegacy(mask, v)
	r := Record{
		Name: l.Name, Kind: l.Kind, Message: l.Message, Usage: l.Usage, DiscardedUsage: l.DiscardedUsage,
		ToolUseID: l.ToolUseID, Result: l.Result, IsError: l.IsError, Approved: l.Approved,
		AttemptedAt: l.AttemptedAt, Reconciled: l.Reconciled, OutcomeUnknown: l.OutcomeUnknown, Redacted: l.Redacted, stamped: l.stamped,
		Safety: l.Safety, Approval: l.Approval, Evidence: l.Evidence, Format: l.Format, claim: l.claim, salt: l.salt,
	}
	if t := (ModelTurn{Finish: l.Finish, RawFinish: l.RawFinish, Model: l.Model, PromptDigest: l.PromptDigest, ToolsDigest: l.ToolsDigest}); t != (ModelTurn{}) || mask&(1<<idForceTurn) != 0 {
		r.ModelTurn = &t
	}
	if l.Approver != "" || l.ApproverAlg != "" || l.Signature != nil || l.DecidedAt != "" || l.RecordedAt != "" || len(l.ApproverCert) != 0 || mask&(1<<idForceSig) != 0 {
		r.ApproverSignature = &ApproverSignature{Approver: l.Approver, ApproverAlg: l.ApproverAlg, Signature: l.Signature, DecidedAt: l.DecidedAt, RecordedAt: l.RecordedAt, ApproverCert: l.ApproverCert}
	}
	return r, l
}

// idLegacy builds the legacyRecord a mask and values describe.
func idLegacy(mask uint32, v idVals) legacyRecord {
	on := func(b int) bool { return mask&(1<<b) != 0 }
	str := func(b int) string {
		if !on(b) {
			return ""
		}
		return v.s[b%len(v.s)]
	}
	var l legacyRecord
	l.Name, l.Kind = str(idName), StepKind(str(idKind))
	if on(idMessage) {
		l.Message = &Message{Role: RoleAssistant, Parts: []Part{Text{Text: v.s[0]}, Reasoning{Text: v.s[1], Signature: v.s[2]}, ToolUse{ID: v.s[3], Name: v.s[4], Args: v.raw, Signature: v.s[5]}}}
	}
	if on(idUsage) {
		l.Usage = &Usage{InputTokens: 3, OutputTokens: int(v.n % 1000), CacheReadTokens: 1}
	}
	if on(idDiscarded) {
		l.DiscardedUsage = &Usage{InputTokens: 5, CacheWriteTokens: 2}
	}
	l.Finish, l.RawFinish = FinishReason(str(idFinish)), str(idRawFinish)
	if on(idModel) {
		l.Model = &ModelInfo{Provider: v.s[0], Model: v.s[1], ResponseFormat: v.single}
	}
	l.PromptDigest, l.ToolsDigest, l.ToolUseID = str(idPromptDigest), str(idToolsDigest), str(idToolUseID)
	if on(idResult) {
		l.Result = v.raw
	}
	l.IsError, l.Approved = on(idIsError), on(idApproved)
	l.Approver, l.ApproverAlg = str(idApprover), Alg(str(idApproverAlg))
	if on(idSignature) {
		l.Signature = v.bin
	}
	l.DecidedAt, l.RecordedAt = str(idDecidedAt), str(idRecordedAt)
	if on(idAttemptedAt) {
		l.AttemptedAt = v.n
	}
	l.Reconciled, l.OutcomeUnknown, l.Redacted, l.stamped = on(idReconciled), on(idOutcomeUnknown), on(idRedacted), on(idStamped)
	if on(idSafety) {
		l.Safety = &Safety{ReadOnly: v.single, Idempotent: true}
	}
	if on(idApproval) {
		l.Approval = &ApprovalPolicy{Need: 2, Approvers: []string{v.s[0], v.s[3], v.s[4]}}
		if v.single {
			l.Approval = SingleApproval()
		}
	}
	if on(idEvidence) {
		l.Evidence = v.raw
	}
	l.Format, l.claim = str(idFormat), str(idClaim)
	if on(idSalt) {
		l.salt = v.bin
	}
	return l
}

// checkIdentity checks r against l, its legacyRecord: the same encodings (MarshalJSON's and
// EncodeRecord's, or the same failure), and the canonical bytes decode, through the record and
// through the legacy layout, to the same content, which encodes back to those bytes. full false
// checks the encodings only.
func checkIdentity(r Record, l legacyRecord, full bool) error {
	mb, merr := marshalRecord(r)
	lmb, lmerr := legacyMarshal(l)
	if (merr == nil) != (lmerr == nil) || !bytes.Equal(mb, lmb) {
		return fmt.Errorf("marshal: %q (%v), legacy %q (%v)", mb, merr, lmb, lmerr)
	}
	b, err := EncodeRecord(r)
	lb, lerr := legacyEncode(l)
	if (err == nil) != (lerr == nil) || !bytes.Equal(b, lb) {
		return fmt.Errorf("EncodeRecord: %q (%v), legacy %q (%v)", b, err, lb, lerr)
	}
	if err != nil || !full {
		return nil
	}
	return checkDecode(lb)
}

// checkDecode checks that b, a legacy journal encoding, decodes through Record to the content it
// decodes to through the legacy layout, and that the record encodes back to b.
func checkDecode(b []byte) error {
	got, err := DecodeRecord(b)
	want, lerr := legacyUnmarshal(b)
	if (err == nil) != (lerr == nil) {
		return fmt.Errorf("decode %q: %v, legacy %v", b, err, lerr)
	}
	if err != nil {
		return nil
	}
	if !bytes.Equal(got.Raw(), b) {
		return fmt.Errorf("decode %q: Raw %q", b, got.Raw())
	}
	view := legacyView(got)
	view.raw = nil
	if !reflect.DeepEqual(view, want) {
		return fmt.Errorf("decode %q:\n got %+v\nwant %+v", b, view, want)
	}
	if (got.ModelTurn != nil) != (want.Finish != "" || want.RawFinish != "" || want.Model != nil || want.PromptDigest != "" || want.ToolsDigest != "") {
		return fmt.Errorf("decode %q: ModelTurn %+v for a record whose journal encoding holds none (or the reverse)", b, got.ModelTurn)
	}
	if (got.ApproverSignature != nil) != (want.Approver != "" || want.ApproverAlg != "" || len(want.Signature) != 0 || want.DecidedAt != "" || want.RecordedAt != "" || len(want.ApproverCert) != 0) {
		return fmt.Errorf("decode %q: ApproverSignature %+v for a record whose journal encoding holds none (or the reverse)", b, got.ApproverSignature)
	}
	again, err := EncodeRecord(got)
	if err != nil || !bytes.Equal(again, b) {
		return fmt.Errorf("decode %q and encode again: %q (%v)", b, again, err)
	}
	return nil
}

// TestRecordIdentity_LegacyLayout: legacyRecord is the old Record field for field (its fields, in
// order, are Record's with ModelTurn's and ApproverSignature's in their places), so the reference
// the other tests encode with cannot drift from the layout the journal was written in.
func TestRecordIdentity_LegacyLayout(t *testing.T) {
	var want []string
	rt := reflect.TypeFor[Record]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Anonymous {
			for j := range f.Type.Elem().NumField() {
				g := f.Type.Elem().Field(j)
				want = append(want, g.Name+" "+g.Type.String()+" "+string(g.Tag))
			}
			continue
		}
		want = append(want, f.Name+" "+f.Type.String()+" "+string(f.Tag))
	}
	var got []string
	lt := reflect.TypeFor[legacyRecord]()
	for i := range lt.NumField() {
		f := lt.Field(i)
		got = append(got, f.Name+" "+f.Type.String()+" "+string(f.Tag))
	}
	// The flags sit in a different place in memory; the journal encoding does not see them.
	norm := func(fs []string) []string {
		var out []string
		for _, f := range fs {
			if f != "Redacted bool json:\"-\"" && f != "stamped bool " {
				out = append(out, f)
			}
		}
		return out
	}
	if g, w := norm(got), norm(want); !reflect.DeepEqual(g, w) {
		t.Fatalf("legacyRecord fields\n%q\nRecord's, flattened\n%q", g, w)
	}
}

// TestRecordIdentity_Exhaustive encodes every combination of the moved fields, the fields beside
// them in the encoding, and a ModelTurn or ApproverSignature present with no field set (16 bits),
// with every other field all set, all zero, or a random mix, under each set of tricky values, and
// checks the bytes and the decode against the legacy layout.
func TestRecordIdentity_Exhaustive(t *testing.T) {
	core := []int{idDiscarded, idFinish, idRawFinish, idModel, idPromptDigest, idToolsDigest, idToolUseID, idResult,
		idIsError, idApproved, idApprover, idApproverAlg, idSignature, idAttemptedAt, idForceTurn, idForceSig}
	var coreMask uint32
	for _, b := range core {
		coreMask |= 1 << b
	}
	rest := idAll &^ coreMask
	rng := rand.New(rand.NewPCG(1, 2))
	n := 0
	for vi, v := range trickyVals {
		for c := range uint32(1) << len(core) {
			var m uint32
			for i, b := range core {
				if c&(1<<i) != 0 {
					m |= 1 << b
				}
			}
			// Every other field all zero, all set, or a random mix, in turn; the bytes are checked for
			// every mask, the decode for one in eight (and every mask in TestRecordIdentity_AllFields).
			o := [3]uint32{0, rest, rest & rng.Uint32()}[(int(c)+vi)%3]
			r, l := idPair(m|o, v)
			if err := checkIdentity(r, l, c%8 == 0); err != nil {
				t.Fatalf("values %d, mask %#x: %v", vi, m|o, err)
			}
			n++
		}
	}
	t.Logf("%d records checked", n)
}

// TestRecordIdentity_Random checks random combinations of every field.
func TestRecordIdentity_Random(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	n := 20000
	if testing.Short() {
		n = 2000
	}
	for i := range n {
		m := rng.Uint32() & idAll
		v := trickyVals[i%len(trickyVals)]
		r, l := idPair(m, v)
		if err := checkIdentity(r, l, true); err != nil {
			t.Fatalf("mask %#x: %v", m, err)
		}
	}
}

// TestRecordIdentity_AllFields checks every one of the 2^32 masks (every field combination, with
// and without empty sub-structs), each under one of the sets of tricky values in turn, decoding one
// in 4096. At about 20 microseconds a mask that is hours of CPU on every core, so it runs only
// with BIDE_RECORD_IDENTITY_ALL=1, and reports its progress every 2^26 masks.
func TestRecordIdentity_AllFields(t *testing.T) {
	if os.Getenv("BIDE_RECORD_IDENTITY_ALL") != "1" {
		t.Skip("set BIDE_RECORD_IDENTITY_ALL=1 to check all 2^32 field combinations")
	}
	total := uint64(1) << idBits
	var next, done atomic.Uint64
	var failed atomic.Pointer[error]
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Go(func() {
			for failed.Load() == nil {
				start := next.Add(1<<16) - 1<<16
				if start >= total {
					return
				}
				for m := start; m < start+1<<16; m++ {
					r, l := idPair(uint32(m), trickyVals[m%uint64(len(trickyVals))])
					if err := checkIdentity(r, l, m%4096 == 0); err != nil {
						err = fmt.Errorf("mask %#x: %w", m, err)
						failed.Store(&err)
						return
					}
				}
				if d := done.Add(1 << 16); d%(1<<26) == 0 {
					t.Logf("%d/%d masks checked", d, total)
				}
			}
		})
	}
	wg.Wait()
	if p := failed.Load(); p != nil {
		t.Fatal(*p)
	}
	t.Logf("%d masks checked", done.Load())
}

// FuzzRecordIdentity checks arbitrary field combinations with arbitrary values.
func FuzzRecordIdentity(f *testing.F) {
	for i, v := range trickyVals {
		f.Add(uint32(0x3fffffff), v.s[0], v.s[1], v.s[2], []byte(v.raw), v.bin, v.n, i%2 == 0)
		f.Add(uint32(0x15555555), v.s[3], v.s[4], v.s[5], []byte(v.raw), v.bin, v.n, i%2 == 1)
	}
	f.Fuzz(func(t *testing.T, mask uint32, s0, s1, s2 string, raw, bin []byte, n int64, single bool) {
		v := idVals{s: [6]string{s0, s1, s2, s1 + s0, s2 + s1, s0 + s2}, raw: raw, bin: bin, n: n, single: single}
		if !json.Valid(raw) {
			v.raw, _ = json.Marshal(string(raw))
		}
		r, l := idPair(mask&idAll, v)
		if err := checkIdentity(r, l, true); err != nil {
			t.Fatal(err)
		}
	})
}

// FuzzRecordIdentity_Decode decodes arbitrary bytes through the record and the legacy layout.
func FuzzRecordIdentity_Decode(f *testing.F) {
	for _, line := range legacyCorpus(f) {
		f.Add(line)
	}
	f.Add([]byte(`{"name":"a","kind":"model","finish":"","approver":"","signature":null,"model":null}`))
	f.Add([]byte(`{"name":"a","kind":"approval","approver":"x","approver_alg":"ed25519","signature":"AQI=","approval":{"single":true,"need":3}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := DecodeRecord(b)
		want, lerr := legacyUnmarshal(b)
		if (err == nil) != (lerr == nil) {
			t.Fatalf("decode %q: %v, legacy %v", b, err, lerr)
		}
		if err != nil {
			return
		}
		if len(want.Signature) == 0 && want.Approver == "" && want.ApproverAlg == "" && want.DecidedAt == "" && want.RecordedAt == "" && len(want.ApproverCert) == 0 {
			// "signature":"" decodes to an empty, non-nil slice in the legacy layout; the record
			// carries no ApproverSignature then, so it reads nil. Both encode alike (omitempty).
			want.Signature = nil
		}
		view := legacyView(got)
		view.raw = nil
		if !reflect.DeepEqual(view, want) {
			t.Fatalf("decode %q:\n got %+v\nwant %+v", b, view, want)
		}
		if (got.ModelTurn != nil) != (want.Finish != "" || want.RawFinish != "" || want.Model != nil || want.PromptDigest != "" || want.ToolsDigest != "") {
			t.Fatalf("decode %q: ModelTurn %+v, though the record holds no model-turn metadata (or the reverse)", b, got.ModelTurn)
		}
		if (got.ApproverSignature != nil) != (want.Approver != "" || want.ApproverAlg != "" || len(want.Signature) != 0 || want.DecidedAt != "" || want.RecordedAt != "" || len(want.ApproverCert) != 0) {
			t.Fatalf("decode %q: ApproverSignature %+v, though the record holds no signed decision (or the reverse)", b, got.ApproverSignature)
		}
		enc, err := EncodeRecord(got)
		lenc, lerr := legacyEncode(want)
		if (err == nil) != (lerr == nil) || !bytes.Equal(enc, lenc) {
			t.Fatalf("re-encode %q: %q (%v), legacy %q (%v)", b, enc, err, lenc, lerr)
		}
	})
}

// legacyCorpusFile holds records the code before ModelTurn and ApproverSignature encoded: one per
// line, each line its EncodeRecord bytes. It was written by the generator in this file's history
// (see TestRecordIdentity_LegacyCorpus) from records idPair built, under each set of tricky values,
// for every mask of the 16 bits TestRecordIdentity_Exhaustive varies in steps of 97 and 64 random
// masks of every field.
const legacyCorpusFile = "testdata/record_identity_legacy.jsonl"

func legacyCorpus(tb testing.TB) [][]byte {
	f, err := os.Open(legacyCorpusFile)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line := sc.Bytes(); len(line) > 0 && line[0] != '#' {
			out = append(out, bytes.Clone(line))
		}
	}
	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}
	return out
}

// TestRecordIdentity_LegacyCorpus: every journal line the code before the move wrote decodes to the
// same content through the record as through the legacy layout, and encodes back to the same bytes.
// The lines are those legacyCorpusMasks builds: the record idPair builds for each one encodes to
// that line today too.
func TestRecordIdentity_LegacyCorpus(t *testing.T) {
	lines := legacyCorpus(t)
	masks := legacyCorpusMasks()
	if len(lines) != len(masks) {
		t.Fatalf("%s has %d records, the generator builds %d", legacyCorpusFile, len(lines), len(masks))
	}
	for i, line := range lines {
		if err := checkDecode(line); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		r, _ := idPair(masks[i].mask, trickyVals[masks[i].vals])
		b, err := EncodeRecord(r)
		if err != nil || !bytes.Equal(b, line) {
			t.Fatalf("line %d (mask %#x, values %d): the record encodes to %q (%v), the old code wrote %q", i+1, masks[i].mask, masks[i].vals, b, err, line)
		}
	}
	t.Logf("%d legacy records checked", len(lines))
}

type corpusMask struct {
	mask uint32
	vals int
}

// legacyCorpusMasks is the masks and value sets the corpus was generated from, in order.
func legacyCorpusMasks() []corpusMask {
	core := []int{idDiscarded, idFinish, idRawFinish, idModel, idPromptDigest, idToolsDigest, idToolUseID, idResult,
		idIsError, idApproved, idApprover, idApproverAlg, idSignature, idAttemptedAt}
	var out []corpusMask
	rng := rand.New(rand.NewPCG(5, 6))
	for vi := range trickyVals {
		for c := uint32(0); c < 1<<len(core); c += 97 {
			m := uint32(1<<idName | 1<<idKind)
			for i, b := range core {
				if c&(1<<i) != 0 {
					m |= 1 << b
				}
			}
			out = append(out, corpusMask{m, vi})
		}
		for range 64 {
			out = append(out, corpusMask{rng.Uint32() & (1<<idDecidedAt - 1), vi})
		}
	}
	return out
}

// The accessors read through a missing sub-struct as the zero value, as the fields read on a
// record that carried none.
func TestRecordAccessors_NilSafe(t *testing.T) {
	var r Record
	if r.Finish() != "" || r.RawFinish() != "" || r.Model() != nil || r.PromptDigest() != "" || r.ToolsDigest() != "" ||
		r.Approver() != "" || r.ApproverAlg() != "" || r.Signature() != nil {
		t.Fatalf("a record with no ModelTurn or ApproverSignature reads %q %q %v %q %q %q %q %v", r.Finish(), r.RawFinish(), r.Model(),
			r.PromptDigest(), r.ToolsDigest(), r.Approver(), r.ApproverAlg(), r.Signature())
	}
	if modelTurn(ModelTurn{}) != nil {
		t.Fatal("modelTurn of no metadata is not nil")
	}
	m := &ModelInfo{Provider: "p"}
	if got := modelTurn(ModelTurn{Model: m}); got == nil || got.Model != m {
		t.Fatalf("modelTurn dropped the metadata: %+v", got)
	}
}

// Record is copied by value throughout the engine, and every heap copy of one (a decode, an
// encode, a store's put) is allocated in the Go size class that holds it. cmd/bench's contended
// overhead scenario is sensitive to its size: padding it by 16 bytes, all the Safety and Approval
// fields first added, shifted the run loop's scheduling under contention (p50 down, p99 up two to
// three times, and wall-clock up by tens of percent on a 4-vCPU runner), and growing it from the
// 288-byte size class to the 384-byte one cost 3 to 8% throughput. The model-turn metadata
// (ModelTurn) and a signed decision's fields (ApproverSignature) sit behind pointers, which brings
// it to 256 bytes, exactly the 256-byte size class. Its size is pinned, so a change that grows it
// says why and is measured with the bench workflow in A/B mode.
func TestRecord_Size(t *testing.T) {
	if n := unsafe.Sizeof(Record{}); n > 256 {
		t.Fatalf("Record is %d bytes, over the 256 it is held to; see the comment", n)
	}
}

// normRaw is the journal's spelling of a raw JSON value: compacted, with U+2028/U+2029 escaped.
func normRaw(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, r); err != nil {
		return r
	}
	b := bytes.ReplaceAll(buf.Bytes(), []byte(lineSep), []byte(jsonEscape+"2028"))
	return bytes.ReplaceAll(b, []byte(paraSep), []byte(jsonEscape+"2029"))
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// normRecord maps a record to the form the journal keeps: raw JSON in its journal spelling, and
// empty slices as nil (omitempty drops them).
func normRecord(r Record) Record {
	r.raw = nil // the bytes it was decoded from, not its content
	r.Result = normRaw(r.Result)
	r.Evidence = normRaw(r.Evidence)
	if r.ApproverSignature != nil {
		s := *r.ApproverSignature
		s.Signature = nilIfEmpty(s.Signature)
		if s.Approver == "" && s.ApproverAlg == "" && s.Signature == nil {
			r.ApproverSignature = nil // a decoded record carries none when the journal holds none
		} else {
			r.ApproverSignature = &s
		}
	}
	if r.Message != nil {
		m := Message{Role: r.Message.Role}
		for _, p := range r.Message.Parts {
			switch v := p.(type) {
			case ToolUse:
				v.Args = normRaw(v.Args)
				p = v
			case ToolResult:
				v.Result = normRaw(v.Result)
				p = v
			case Image:
				v.Data = nilIfEmpty(v.Data)
				p = v
			}
			m.Parts = append(m.Parts, p)
		}
		r.Message = &m
	}
	return r
}

// FuzzRecordRoundTrip: for an arbitrary record built from valid-UTF-8 strings and valid raw JSON,
// decode(encode(r)) equals r in every field the journal keeps, and encode(decode(b)) == b.
func FuzzRecordRoundTrip(f *testing.F) {
	f.Add("n", "a <b> & c", []byte(`{ "h" : "<b>" }`), []byte("\x00\xff"), uint8(0), int64(5), true)
	f.Add(lineSep, "x"+paraSep, []byte(`[1.50, 1e20, -0.0]`), []byte{}, uint8(63), int64(-1), false)
	f.Add("", "", []byte(`null`), []byte(nil), uint8(255), int64(0), false)
	f.Add("\U0001F600", `"\ud800"`, []byte(`{"k":"\ud800","a":"`+lineSep+`"}`), []byte{0xd3}, uint8(62), int64(1<<40), true)
	kinds := []StepKind{StepModel, StepToolResult, StepValue, StepSignal, StepApproval, StepAttempt, StepSagaFail, ""}
	f.Fuzz(func(t *testing.T, s1, s2 string, raw, bin []byte, sel uint8, n int64, flag bool) {
		if !utf8.ValidString(s1) || !utf8.ValidString(s2) {
			return // encoding rewrites invalid UTF-8 to U+FFFD by design; audit refuses such records
		}
		if len(raw) > 0 && !json.Valid(raw) {
			raw = nil
		}
		var parts []Part
		if sel&1 != 0 {
			parts = append(parts, Text{Text: s1})
		}
		if sel&2 != 0 {
			parts = append(parts, Reasoning{Text: s2, Signature: s1}, Reasoning{Redacted: s2})
		}
		if sel&4 != 0 {
			parts = append(parts, ToolUse{ID: s1, Name: s2, Args: raw, Signature: s2})
		}
		if sel&8 != 0 {
			parts = append(parts, ToolResult{ToolUseID: s2, Result: raw, IsError: flag})
		}
		if sel&16 != 0 {
			parts = append(parts, Image{Mime: s1, Data: bin, URL: s2})
		}
		r := Record{Name: s1, Kind: kinds[int(sel>>5)%len(kinds)], ToolUseID: s2, Result: raw, IsError: flag,
			Approved: !flag, AttemptedAt: n, Reconciled: flag, Evidence: raw, claim: s1, ApproverSignature: &ApproverSignature{Approver: s2, Signature: bin}}
		if sel&32 != 0 {
			r.Message = &Message{Role: Role(s2), Parts: parts}
			r.Usage = &Usage{InputTokens: int(n), OutputTokens: int(sel), CacheReadTokens: -int(n), CacheWriteTokens: int(n) / 3}
		}
		b, err := EncodeRecord(r)
		if err != nil {
			t.Fatalf("EncodeRecord of a well-formed record: %v", err)
		}
		back, err := DecodeRecord(b)
		if err != nil {
			t.Fatalf("DecodeRecord(EncodeRecord(r)): %v\n%s", err, b)
		}
		if want, got := normRecord(r), normRecord(back); !reflect.DeepEqual(want, got) {
			t.Fatalf("round trip lost data:\nwant %#v\ngot  %#v\nenc  %s", want, got, b)
		}
		again, err := EncodeRecord(back)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("encode(decode(b)) != b (%v):\n%s\n%s", err, b, again)
		}
	})
}

// FuzzDecodeRecord: arbitrary bytes never panic DecodeRecord, and anything it accepts reaches the
// journal fixed point in one EncodeRecord.
func FuzzDecodeRecord(f *testing.F) {
	for _, s := range []string{
		`{"name":"@llm/0","kind":"model","message":{"role":"assistant","parts":[{"text":"a","type":"text"},{"args":{"q":1},"id":"c1","name":"t","type":"tool_use"},{"data":"AAH/PA==","mime":"image/png","type":"image"}]}}`,
		`{"name":"c1","kind":"tool_result","tool_use_id":"c1","result":{"n":[1e20,-0.0]},"evidence":" "}`,
		`{"message":{"parts":[{"type":"reasoning","text":"t","signature":"s"},{"type":"tool_result","result":null}]}}`,
		// Case-variant and unknown names inside a message part.
		`{"message":{"role":"assistant","parts":[{"type":"text","text":"refund $1","Text":"refund $10","approved_by":"cfo"}]}}`,
		// Non-canonical base64 (unused bits set, a line break).
		`{"kind":"approval","signature":"0x==","message":{"parts":[{"type":"image","mime":"image/png","data":"AB\nC="}]}}`,
		// Escaped lone surrogates, a surrogate pair, and U+2028 raw and escaped.
		`{"name":"run\ud800","result":{"k":"\udc00"},"message":{"parts":[{"type":"text","text":"😀\ud800"}]}}`,
		"{\"name\":\"a b\\u2029\",\"claim\":\"\xff\"}",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := DecodeRecord(b)
		if err != nil {
			return
		}
		enc, err := EncodeRecord(r)
		if err != nil {
			return // e.g. a part whose raw JSON encoding/json refuses to re-emit
		}
		r2, err := DecodeRecord(enc)
		if err != nil {
			t.Fatalf("DecodeRecord(EncodeRecord(decoded)): %v\n%s", err, enc)
		}
		if !reflect.DeepEqual(normRecord(r), normRecord(r2)) {
			t.Fatalf("decode(encode(decoded)) differs:\n%#v\n%#v", r, r2)
		}
		enc2, err := EncodeRecord(r2)
		if err != nil || !bytes.Equal(enc, enc2) {
			t.Fatalf("not a fixed point (%v):\n%s\n%s", err, enc, enc2)
		}
	})
}

// The records that resolve something a person was told about (a single approver's decision,
// ResolveHalt's result, a run's end) note when they were written, in recorded_at. These tests
// specify the time, the clock it comes from, and that nothing reads it.

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

var (
	timesA = time.Date(2026, 10, 14, 9, 30, 15, 123_999_000, time.FixedZone("east", 2*3600)) // 07:30:15.123Z
	timesB = time.Date(2026, 10, 14, 10, 0, 0, 456_000_000, time.UTC)
	timesC = time.Date(2026, 10, 14, 11, 0, 0, 789_000_000, time.UTC)
)

const (
	stampA = "2026-10-14T07:30:15.123Z"
	stampB = "2026-10-14T10:00:00.456Z"
	stampC = "2026-10-14T11:00:00.789Z"
)

// A single approver's decision (Approve, an approval, a denial) carries recorded_at: WithClock's
// clock, else the context's, else the wall clock, in the one canonical form. A repeated call
// keeps the first decision and its time. It is a record with no approver, so it is not an m-of-n
// decision.
func TestRecordTimes_SingleApproval(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if err := Approve(ctx, j, "r", "c1", true, WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	if err := Approve(ctx, j, "r", "c1", true, WithClock(fixedClock(timesB))); err != nil {
		t.Fatal(err)
	}
	if err := Approve(ctx, j, "r", "c2", false, WithClock(fixedClock(timesB))); err != nil {
		t.Fatal(err)
	}
	if err := Approve(contextWithClock(ctx, fixedClock(timesC)), j, "r", "c3", true); err != nil {
		t.Fatal(err)
	}
	// The option beats the context's clock.
	if err := Approve(contextWithClock(ctx, fixedClock(timesC)), j, "r", "c4", true, WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	if err := Approve(ctx, j, "r", "c5", true); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Second)
	for id, want := range map[string]string{"c1": stampA, "c2": stampB, "c3": stampC, "c4": stampA} {
		rec, ok := hasStep(t, j, "r", approvalStep(id))
		if !ok || rec.Kind != StepApproval || rec.RecordedAt() != want {
			t.Errorf("%s: approval %+v (found %v), want recorded_at %q", id, rec, ok, want)
		}
		if rec.Approver() != "" || rec.Signature() != nil || rec.DecidedAt() != "" || IsApprovalDecision(rec, id) {
			t.Errorf("%s: a single approver's decision reads as a signed one: %+v", id, rec)
		}
	}
	if rec, _ := hasStep(t, j, "r", approvalStep("c2")); rec.Approved {
		t.Error("c2: the denial was recorded as an approval")
	}
	rec, _ := hasStep(t, j, "r", approvalStep("c5"))
	at, err := ParseTimestamp(rec.RecordedAt())
	if err != nil || at.Before(before) || at.After(after) {
		t.Errorf("wall clock: recorded_at %q (%v), want a canonical time within the call", rec.RecordedAt(), err)
	}
}

// Approve does not fail for want of a time: a clock with no canonical form records none. A nil
// clock, and a decision check (which belongs to SubmitDecision), are ErrConfig and record nothing.
func TestRecordTimes_SingleApprovalClockFaults(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := Approve(ctx, j, "r", "c1", true, WithClock(fixedClock(far))); err != nil {
		t.Fatalf("Approve with an out-of-range clock = %v, want it recorded without a time", err)
	}
	if rec, ok := hasStep(t, j, "r", approvalStep("c1")); !ok || !rec.Approved || rec.RecordedAt() != "" || rec.ApproverSignature != nil {
		t.Fatalf("approval = %+v (found %v), want an approval with no time", rec, ok)
	}
	for name, opt := range map[string]ApproveOption{
		"nil clock":      WithClock(nil),
		"decision check": WithDecisionCheck(func(string) (ApproverVerifier, bool) { return nil, false }),
	} {
		if err := Approve(ctx, j, "r", "c2", true, opt); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Approve = %v, want ErrConfig", name, err)
		}
		if _, ok := hasStep(t, j, "r", approvalStep("c2")); ok {
			t.Errorf("%s: a refused Approve recorded a decision", name)
		}
	}
	if err := Approve(ctx, j, "r", "c3", true, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Approve with a nil option = %v, want ErrConfig", err)
	}
}

// ResolveHalt's recorded outcome, for a tool call and for a Step, carries recorded_at: WithClock's
// clock, else the wall clock (never the context's, which is the run's: the resolver is not a drive). The resolver signs nothing, so the record has
// no approver or signature.
func TestRecordTimes_ResolveHalt(t *testing.T) {
	ctx := context.Background()
	stepRef := HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "charge"}, Cause: HaltCrashed}
	mk := func() *Journal {
		j := markerAt(t, time.Now().UnixMilli())
		jj := mustJournal(j)
		if _, err := jj.do(ctx, "r", stepAttemptStep("charge"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "charge", AttemptedAt: time.Now().UnixMilli()}, nil
		}); err != nil {
			t.Fatal(err)
		}
		return jj
	}
	for name, c := range map[string]struct {
		ref   HaltRef
		key   string
		ctx   context.Context
		opts  []ResolveOption
		want  string
		recon bool
	}{
		"tool, WithClock":        {covHalt, ToolResultStep("c1"), ctx, []ResolveOption{WithClock(fixedClock(timesA))}, stampA, false},
		"tool, option beats ctx": {covHalt, ToolResultStep("c1"), contextWithClock(ctx, fixedClock(timesB)), []ResolveOption{WithClock(fixedClock(timesC))}, stampC, false},
		"step, WithClock":        {stepRef, "charge", ctx, []ResolveOption{WithClock(fixedClock(timesA))}, stampA, false},
		"reconciled":             {covHalt, ToolResultStep("c1"), ctx, []ResolveOption{WithEvidence("provider says charged"), WithClock(fixedClock(timesC))}, stampC, true},
	} {
		j := mk()
		if err := ResolveHalt(c.ctx, j, c.ref, Outcome{Result: "charged"}, append(c.opts, WithoutLiveDriverCheck())...); err != nil {
			t.Fatalf("%s: ResolveHalt = %v", name, err)
		}
		rec, ok := hasStep(t, j, "r", c.key)
		if !ok || rec.RecordedAt() != c.want || rec.Reconciled != c.recon {
			t.Errorf("%s: result %+v (found %v), want recorded_at %q, reconciled %v", name, rec, ok, c.want, c.recon)
		}
		if rec.Approver() != "" || rec.Signature() != nil {
			t.Errorf("%s: the resolver's record claims a signature: %+v", name, rec)
		}
		// Resolving again with the same outcome is a no-op that keeps the first time.
		if err := ResolveHalt(contextWithClock(ctx, fixedClock(timesC.Add(time.Hour))), j, c.ref, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithClock(fixedClock(timesC.Add(time.Hour)))); err != nil {
			t.Fatalf("%s: second ResolveHalt = %v", name, err)
		}
		if again, _ := hasStep(t, j, "r", c.key); again.RecordedAt() != c.want {
			t.Errorf("%s: a repeated resolution changed recorded_at to %q", name, again.RecordedAt())
		}
	}
	// The wall clock, when nothing names one.
	j := mk()
	before := time.Now().Add(-time.Second)
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	rec, _ := hasStep(t, j, "r", ToolResultStep("c1"))
	if at, err := ParseTimestamp(rec.RecordedAt()); err != nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("wall clock: recorded_at %q (%v)", rec.RecordedAt(), err)
	}
	// A clock with no canonical form records no time, and the resolution still happens.
	j = mk()
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithClock(fixedClock(far))); err != nil {
		t.Fatalf("ResolveHalt with an out-of-range clock = %v", err)
	}
	if rec, ok := hasStep(t, j, "r", ToolResultStep("c1")); !ok || rec.RecordedAt() != "" || rec.ApproverSignature != nil {
		t.Errorf("out-of-range clock: %+v (found %v), want a result with no time", rec, ok)
	}
}

// The context's clock neither stamps the resolution nor measures a halt's age: WithMinHaltAge
// compares the attempt marker's wall-clock time with the clock WithClock names, else the wall
// clock, as it did before the resolution carried a time.
func TestRecordTimes_ResolveHaltAgeIgnoresContextClock(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(markerAt(t, time.Now().UnixMilli()))
	future := contextWithClock(ctx, fixedClock(time.Now().Add(240*time.Hour)))
	err := ResolveHalt(future, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithMinHaltAge(time.Hour))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("ResolveHalt with a context clock ten days ahead = %v, want *HaltTooYoung (the age is the wall clock's)", err)
	}
	if _, ok := hasStep(t, j, "r", ToolResultStep("c1")); ok {
		t.Fatal("a refused resolution recorded a result")
	}
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck(), WithMinHaltAge(time.Hour), WithClock(fixedClock(time.Now().Add(2*time.Hour)))); err != nil {
		t.Fatalf("ResolveHalt with WithClock two hours ahead = %v, want it resolved", err)
	}
}

// A run's completion records recorded_at, by the agent's clock, the run's, or the wall clock, and
// Status reports it as EndedAt. A drive that finds the run complete writes no second marker and
// keeps the time.
func TestRecordTimes_RunComplete(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		ctx  context.Context
		opts []Option
		want string
	}{
		"agent clock":   {ctx, []Option{WithClock(fixedClock(timesA))}, stampA},
		"context clock": {contextWithClock(ctx, fixedClock(timesB)), nil, stampB},
	} {
		j := memJournal()
		var n int
		a := mustNew(lookupRefund, j, append([]Option{refundTools(&n, &n, nil)}, c.opts...)...)
		if _, err := a.Run(c.ctx, "r", UserText("go")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rec, ok := hasStep(t, j, "r", runCompleteStep)
		if !ok || rec.RecordedAt() != c.want {
			t.Errorf("%s: run:complete %+v (found %v), want recorded_at %q", name, rec, ok, c.want)
		}
		if st, err := Status(ctx, j, "r"); err != nil || st.State != RunCompleted || st.EndedAt != c.want {
			t.Errorf("%s: Status = %+v, %v, want completed at %q", name, st, err, c.want)
		}
		// A later drive of the finished run keeps the first marker.
		if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil), WithClock(fixedClock(timesC))).Run(ctx, "r", UserText("go")); err != nil {
			t.Fatalf("%s: redrive: %v", name, err)
		}
		if again, _ := hasStep(t, j, "r", runCompleteStep); again.RecordedAt() != c.want {
			t.Errorf("%s: a later drive changed run:complete's time to %q", name, again.RecordedAt())
		}
	}
}

// Cancel records recorded_at on run:cancelled (WithClock, else the context's clock). On a saga it
// stamps the rollback request when requested, and the run:cancelled the finished rollback writes
// when the rollback finished, so Status reports the end, not the request.
func TestRecordTimes_Cancel(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "plain", false)
	if err := Cancel(ctx, j, "plain", "stop", WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	pcStart(t, j, "viactx", false)
	if err := Cancel(contextWithClock(ctx, fixedClock(timesB)), j, "viactx", "stop"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"plain": stampA, "viactx": stampB} {
		rec, ok := hasStep(t, j, id, runCancelledStep)
		if !ok || rec.RecordedAt() != want {
			t.Errorf("%s: run:cancelled %+v (found %v), want recorded_at %q", id, rec, ok, want)
		}
		if st, _ := Status(ctx, j, id); st.State != RunCancelled || st.EndedAt != want || st.Terminal != "stop" {
			t.Errorf("%s: Status = %+v, want cancelled at %q", id, st, want)
		}
		var v cancelReason
		if err := json.Unmarshal(rec.Result, &v); err != nil || v.Reason != "stop" {
			t.Errorf("%s: the reason moved: %s (%v)", id, rec.Result, err)
		}
	}
	// Cancelling a cancelled run again keeps the first time.
	if err := Cancel(ctx, j, "plain", "again", WithClock(fixedClock(timesC))); err != nil {
		t.Fatal(err)
	}
	if rec, _ := hasStep(t, j, "plain", runCancelledStep); rec.RecordedAt() != stampA {
		t.Errorf("a second Cancel changed the time to %q", rec.RecordedAt())
	}
	// A saga: the request has its time, and the rollback's end marker its own.
	pcStart(t, j, "saga", true)
	if err := Cancel(ctx, j, "saga", "stop", WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	if req, ok := hasStep(t, j, "saga", runCancelRequestedStep); !ok || req.RecordedAt() != stampA {
		t.Errorf("run:cancel-requested %+v (found %v), want recorded_at %q", req, ok, stampA)
	}
	if st, _ := Status(ctx, j, "saga"); st.State != RunStarted || st.EndedAt != "" {
		t.Errorf("Status of a saga with a pending request = %+v, want started with no end time", st)
	}
	for name, opt := range map[string]CancelOption{"nil clock": WithClock(nil)} {
		if err := Cancel(ctx, j, "plain", "x", opt); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Cancel = %v, want ErrConfig", name, err)
		}
	}
	if err := Cancel(ctx, j, "plain", "x", nil); !errors.Is(err, ErrConfig) {
		t.Errorf("Cancel with a nil option = %v, want ErrConfig", err)
	}
}

// A saga's rollback writes run:aborted or run:cancelled with the rollback's own time: by the
// drive's clock, once, so aborted_at and recorded_at agree.
func TestRecordTimes_SagaEnd(t *testing.T) {
	ctx := contextWithClock(context.Background(), fixedClock(timesB))
	j := memJournal()
	a := mustNew(turnModel{toolTurn("c1", "boom", `{}`), textTurn("x")}, j, WithTools(failTool("boom")))
	_, err := a.Run(ctx, "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("Run = %v, want *SagaAborted", err)
	}
	rec, ok := hasStep(t, j, "r", runAbortedStep)
	if !ok || rec.RecordedAt() != stampB {
		t.Fatalf("run:aborted %+v (found %v), want recorded_at %q", rec, ok, stampB)
	}
	var v abortValue
	if err := json.Unmarshal(rec.Result, &v); err != nil || v.AbortedAt != stampB || v.Reason == "" {
		t.Fatalf("run:aborted value %s: %+v (%v), want aborted_at %q beside the reason", rec.Result, v, err, stampB)
	}
	if st, _ := Status(ctx, j, "r"); st.State != RunAborted || st.EndedAt != stampB {
		t.Fatalf("Status = %+v, want aborted at %q", st, stampB)
	}
}

// The aborted_at and recorded_at of a saga's run:aborted come from one reading of the clock, so
// they agree even when the clock moves between calls.
func TestRecordTimes_SagaEndOneReading(t *testing.T) {
	var n int
	tick := func() time.Time { n++; return timesB.Add(time.Duration(n) * time.Second) }
	ctx := contextWithClock(context.Background(), tick)
	j := memJournal()
	a := mustNew(turnModel{toolTurn("c1", "boom", `{}`), textTurn("x")}, j, WithTools(failTool("boom")))
	if _, err := a.Run(ctx, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("want the saga to abort")
	}
	rec, _ := hasStep(t, j, "r", runAbortedStep)
	var v abortValue
	if err := json.Unmarshal(rec.Result, &v); err != nil || v.AbortedAt == "" || rec.RecordedAt() != v.AbortedAt {
		t.Fatalf("aborted_at %q and recorded_at %q differ (%v)", v.AbortedAt, rec.RecordedAt(), err)
	}
}

// Status never supplies a time: an end marker written before they carried one reports none, a
// run:aborted that has only its aborted_at reports that, and a recorded_at that is not canonical
// is not reported.
func TestRecordTimes_StatusNeverInvents(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "old", false)
	pcMark(t, j, "old", runCompleteStep, "")
	if st, _ := Status(ctx, j, "old"); st.State != RunCompleted || st.EndedAt != "" {
		t.Errorf("an end marker with no time: Status = %+v, want no EndedAt", st)
	}
	pcStart(t, j, "oldabort", true)
	if _, err := j.put(ctx, "oldabort", runAbortedStep, Record{Kind: StepValue, Result: []byte(`{"reason":"x","aborted_at":"` + stampC + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if st, _ := Status(ctx, j, "oldabort"); st.EndedAt != stampC {
		t.Errorf("a run:aborted with only aborted_at: EndedAt = %q, want %q", st.EndedAt, stampC)
	}
	pcStart(t, j, "oldstr", true)
	pcMark(t, j, "oldstr", runAbortedStep, "step failed")
	if st, _ := Status(ctx, j, "oldstr"); st.State != RunAborted || st.EndedAt != "" || st.Terminal != "step failed" {
		t.Errorf("a run:aborted that is a bare string: Status = %+v", st)
	}
	for i, bad := range []string{"yesterday", "2026-10-14T07:30:15Z", "2026-10-14T07:30:15.123+00:00", " "} {
		id := fmt.Sprintf("bad%d", i)
		pcStart(t, j, id, false)
		if _, err := j.put(ctx, id, runCancelledStep, Record{Kind: StepValue, Result: []byte(`{"reason":"x"}`), ApproverSignature: &ApproverSignature{RecordedAt: bad}}); err != nil {
			t.Fatal(err)
		}
		if st, _ := Status(ctx, j, id); st.State != RunCancelled || st.EndedAt != "" {
			t.Errorf("recorded_at %q: Status = %+v, want cancelled with no EndedAt", bad, st)
		}
	}
}

// No drive, gate, claim or recovery pass reads recorded_at: the same journal driven on with the
// time as written, absent, different, and not a time does and journals the same.
func TestRecordTimes_NotRead(t *testing.T) {
	ctx := context.Background()
	variants := map[string]func(*ApproverSignature) *ApproverSignature{
		"as written": func(s *ApproverSignature) *ApproverSignature { return s },
		"absent":     func(*ApproverSignature) *ApproverSignature { return nil },
		"different": func(*ApproverSignature) *ApproverSignature {
			return &ApproverSignature{RecordedAt: "1999-01-01T00:00:00.000Z"}
		},
		"far future": func(*ApproverSignature) *ApproverSignature {
			return &ApproverSignature{RecordedAt: "9999-12-31T23:59:59.999Z"}
		},
		"not a time": func(*ApproverSignature) *ApproverSignature {
			return &ApproverSignature{RecordedAt: "not a time \x00 ‮"}
		},
	}
	// rewrite replaces the stamp on every record of run "r" that carries a lone recorded_at or is
	// named in names.
	rewrite := func(t *testing.T, src *Journal, names map[string]bool, edit func(*ApproverSignature) *ApproverSignature) *Journal {
		t.Helper()
		out := memJournal()
		recs, err := src.History(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Kind == StepHeader {
				continue
			}
			if names[r.Name] || r.ApproverSignature != nil && r.Approver() == "" {
				r.ApproverSignature = edit(r.ApproverSignature)
			}
			r.salt, r.raw = nil, nil
			if _, err := out.put(ctx, "r", r.Name, r); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	type outcome struct {
		text             string
		err              string
		lookups, refunds int
	}
	cases := map[string]struct {
		setup func(t *testing.T) *Journal
		drive func(t *testing.T, j *Journal) outcome
		names []string
	}{
		"single approval": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, SingleApproval())).Run(ctx, "r", UserText("go")); !isPending(err) {
					t.Fatalf("setup: %v", err)
				}
				if err := Approve(ctx, j, "r", "c2", true, WithClock(fixedClock(timesA))); err != nil {
					t.Fatal(err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, SingleApproval())).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				return o
			},
			names: []string{approvalStep("c2")},
		},
		"single denial": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, SingleApproval())).Run(ctx, "r", UserText("go")); !isPending(err) {
					t.Fatalf("setup: %v", err)
				}
				if err := Approve(ctx, j, "r", "c2", false, WithClock(fixedClock(timesA))); err != nil {
					t.Fatal(err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, SingleApproval())).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				if o.refunds != 0 {
					t.Errorf("a denied refund ran")
				}
				return o
			},
			names: []string{approvalStep("c2")},
		},
		"resolved halt": {
			setup: func(t *testing.T) *Journal {
				s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c2")}
				j := mustJournal(s)
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
					t.Fatalf("setup: %v", err)
				}
				if err := ResolveHalt(ctx, mustJournal(s.Store), HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c2", ToolName: "refund"}, Cause: HaltCrashed}, Outcome{Result: "refunded elsewhere"}, WithClock(fixedClock(timesA)), WithoutLiveDriverCheck()); err != nil {
					t.Fatal(err)
				}
				return mustJournal(s.Store)
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil)).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				return o
			},
			names: []string{ToolResultStep("c2")},
		},
		"finished run": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil), WithClock(fixedClock(timesA))).Run(ctx, "r", UserText("go")); err != nil {
					t.Fatal(err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil)).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				if done, err := IsComplete(ctx, j, "r"); err != nil || !done {
					t.Errorf("IsComplete = %v, %v", done, err)
				}
				return o
			},
			names: []string{runCompleteStep},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := c.setup(t)
			names := map[string]bool{}
			for _, n := range c.names {
				names[n] = true
			}
			var want outcome
			for _, v := range []string{"as written", "absent", "different", "far future", "not a time"} {
				j := rewrite(t, src, names, variants[v])
				got := c.drive(t, j)
				if v == "as written" {
					want = got
				} else if got != want {
					t.Errorf("recorded_at %s: %+v, want %+v", v, got, want)
				}
			}
		})
	}
}

func isPending(err error) bool {
	_, ok := errors.AsType[*ApprovalPending](err)
	return ok
}

// A record that carries recorded_at alone is written and read back as itself: the encoding holds
// one top-level member and nothing for the signature, a decoded record encodes to the same bytes,
// and one written without it decodes with none. A time-only stamp is not a signed decision.
func TestRecordTimes_Encoding(t *testing.T) {
	rec := Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{RecordedAt: stampA}}
	b, err := EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"approval:c1","kind":"approval","tool_use_id":"c1","approved":true,"recorded_at":"` + stampA + `"}`
	if string(b) != want {
		t.Fatalf("encoding = %s, want %s", b, want)
	}
	for _, m := range []string{"approver", "signature", "decided_at"} {
		if bytes.Contains(b, []byte(m)) {
			t.Fatalf("a time-only stamp wrote %q: %s", m, b)
		}
	}
	back, err := DecodeStoredRecord("r", "approval:c1", b)
	if err != nil {
		t.Fatal(err)
	}
	if back.RecordedAt() != stampA || back.Approver() != "" || back.DecidedAt() != "" || !back.Approved {
		t.Fatalf("decoded %+v", back)
	}
	if again, _ := EncodeRecord(back); !bytes.Equal(again, b) {
		t.Fatalf("decode then encode changed the bytes:\n %s\n %s", b, again)
	}
	// A record written before the time existed has none, and gains none on a round trip.
	old, err := decodeTimes(`{"name":"approval:c1","kind":"approval","tool_use_id":"c1","approved":true}`)
	if err != nil || old.RecordedAt() != "" || old.ApproverSignature != nil {
		t.Fatalf("old approval %+v, %v: want no time", old, err)
	}
	if again, _ := EncodeRecord(old); strings.Contains(string(again), "recorded_at") {
		t.Fatalf("a round trip invented a time: %s", again)
	}
	empty, err := decodeTimes(`{"name":"run:complete","kind":"value","recorded_at":""}`)
	if err != nil || empty.ApproverSignature != nil {
		t.Fatalf("an empty recorded_at left a signature: %+v, %v", empty, err)
	}
}

func decodeTimes(js string) (Record, error) {
	var name struct{ Name string }
	_ = json.Unmarshal([]byte(js), &name)
	return DecodeStoredRecord("r", name.Name, []byte(js))
}

// An interrupt answer and a signal note when they were written: WithClock's clock, else the
// context's, else the wall clock; a redelivery keeps the first time; a nil clock or option is
// ErrConfig and records nothing.
func TestRecordTimes_AnswersAndSignals(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if err := j.AnswerInterrupt(ctx, "r", "q", "yes", WithClock(fixedClock(timesA))); err != nil {
		t.Fatal(err)
	}
	if err := j.AnswerInterrupt(ctx, "r", "q", "no", WithClock(fixedClock(timesB))); err != nil {
		t.Fatal(err)
	}
	if err := j.Signal(contextWithClock(ctx, fixedClock(timesB)), "r", "s", 1); err != nil {
		t.Fatal(err)
	}
	if err := j.Signal(ctx, "r", "s2", 1, WithClock(fixedClock(timesC))); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{interruptStep("q"): stampA, signalStep("s"): stampB, signalStep("s2"): stampC} {
		rec, ok := hasStep(t, j, "r", name)
		if !ok || rec.RecordedAt() != want {
			t.Errorf("%s: %+v (found %v), want recorded_at %q", name, rec, ok, want)
		}
	}
	if err := j.Signal(ctx, "r", "bad", 1, WithClock(nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("Signal with a nil clock = %v", err)
	}
	if err := j.AnswerInterrupt(ctx, "r", "bad", 1, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("AnswerInterrupt with a nil option = %v", err)
	}
	if _, ok := hasStep(t, j, "r", signalStep("bad")); ok {
		t.Error("a refused Signal recorded a record")
	}
}

// stripRecordedAt zeroes the recorded_at of every record of rs (and nothing else of a signed
// decision's signature: approver, scheme, signature and decided_at stay, so a comparison still sees
// them), and reports which records carried one.
func stripRecordedAt(rs []Record) []bool {
	had := make([]bool, len(rs))
	for i := range rs {
		s := rs[i].ApproverSignature
		if s == nil {
			continue
		}
		had[i] = s.RecordedAt != ""
		c := *s
		c.RecordedAt = ""
		if c.Approver == "" && c.ApproverAlg == "" && len(c.Signature) == 0 && c.DecidedAt == "" {
			rs[i].ApproverSignature = nil
		} else {
			rs[i].ApproverSignature = &c
		}
	}
	return had
}

// stripRecordedAt clears only the recorded_at, reports which records had one, and leaves a signed
// decision's other fields where they are.
func TestRecordTimes_StripPresence(t *testing.T) {
	rs := []Record{
		{Name: "a"},
		{Name: "b", ApproverSignature: &ApproverSignature{RecordedAt: stampA}},
		{Name: "c", ApproverSignature: &ApproverSignature{Approver: "ops", ApproverAlg: "x", Signature: []byte{1}, DecidedAt: stampB, RecordedAt: stampC}},
		{Name: "d", ApproverSignature: &ApproverSignature{Approver: "ops", DecidedAt: stampB}},
	}
	got := stripRecordedAt(rs)
	if want := []bool{false, true, true, false}; !slices.Equal(got, want) {
		t.Fatalf("presence = %v, want %v", got, want)
	}
	if rs[1].ApproverSignature != nil {
		t.Errorf("a lone stamp left %+v", rs[1].ApproverSignature)
	}
	if s := rs[2].ApproverSignature; s == nil || s.Approver != "ops" || s.DecidedAt != stampB || s.RecordedAt != "" || len(s.Signature) != 1 {
		t.Errorf("a signed decision's stamp strip = %+v, want everything but recorded_at kept", s)
	}
	if s := rs[3].ApproverSignature; s == nil || s.Approver != "ops" {
		t.Errorf("a signed decision with no stamp changed: %+v", s)
	}
}

// REVIEW: the resolution claims its own attempt (AttemptedAt: cfg.now, the wall clock without
// WithClock) and stamps the result with the context's clock: one call, two clocks.
func TestReview_ResolveHaltOneCallOneClock(t *testing.T) {
	ctx := contextWithClock(context.Background(), fixedClock(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)))
	j := mustJournal(markerAt(t, time.Now().Add(-2*time.Hour).UnixMilli()))
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithMinHaltAge(time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, _ := hasStep(t, j, "r", ToolResultStep("c1"))
	recs, _ := j.History(context.Background(), "r")
	var claimed time.Time
	for _, r := range recs {
		if r.Kind == StepAttempt && r.AttemptedAt > 0 && r.Name != toolAttemptStep("c1") {
			claimed = time.UnixMilli(r.AttemptedAt)
		}
	}
	at, err := ParseTimestamp(res.RecordedAt())
	if err != nil || claimed.IsZero() {
		t.Fatalf("recorded_at %q (%v), claim marker %v", res.RecordedAt(), err, claimed)
	}
	if d := claimed.Sub(at); d > time.Minute || d < -time.Minute {
		t.Errorf("one ResolveHalt call: its claim marker says %v, its result says %v", claimed.UTC(), at)
	}
}

// REVIEW: Status falls back to aborted_at only when recorded_at is absent; a garbage recorded_at
// hides a valid aborted_at.
func TestReview_EndedAtFallsBackToAbortedAt(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "x", true)
	v := mustJSONValue(abortValue{Reason: "boom", AbortedAt: "2026-10-14T07:30:15.123Z"})
	if _, err := j.put(ctx, "x", runAbortedStep, Record{Kind: StepValue, Result: v, ApproverSignature: &ApproverSignature{RecordedAt: "garbage"}}); err != nil {
		t.Fatal(err)
	}
	st, err := Status(ctx, j, "x")
	if err != nil || st.EndedAt != "2026-10-14T07:30:15.123Z" {
		t.Errorf("EndedAt = %q (%v), want the valid aborted_at", st.EndedAt, err)
	}
}

// REVIEW: a saga Cancel repeated with another clock keeps one request, first time.
func TestReview_SagaCancelRepeatKeepsFirstTime(t *testing.T) {
	ctx := context.Background()
	j, _ := pcJournal(t)
	pcStart(t, j, "s", true)
	for _, at := range []time.Time{timesA, timesB} {
		if err := Cancel(ctx, j, "s", "stop", WithClock(fixedClock(at))); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := j.History(ctx, "s")
	n := 0
	for _, r := range recs {
		if r.Name == runCancelRequestedStep {
			n++
			if r.RecordedAt() != stampA {
				t.Errorf("recorded_at = %q, want first %q", r.RecordedAt(), stampA)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d requests", n)
	}
}
