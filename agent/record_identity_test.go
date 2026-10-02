package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
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
		Approver: r.Approver(), ApproverAlg: r.ApproverAlg(), Signature: r.Signature(),
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
	idFields    // the number of fields
	idForceTurn = idFields
	idForceSig  = idFields + 1
	idBits      = idFields + 2
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
	if l.Approver != "" || l.ApproverAlg != "" || l.Signature != nil || mask&(1<<idForceSig) != 0 {
		r.ApproverSignature = &ApproverSignature{Approver: l.Approver, ApproverAlg: l.ApproverAlg, Signature: l.Signature}
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
	if (got.ApproverSignature != nil) != (want.Approver != "" || want.ApproverAlg != "" || want.Signature != nil) {
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
	rest := (uint32(1)<<idBits - 1) &^ coreMask
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
		m := rng.Uint32() & (1<<idBits - 1)
		v := trickyVals[i%len(trickyVals)]
		r, l := idPair(m, v)
		if err := checkIdentity(r, l, true); err != nil {
			t.Fatalf("mask %#x: %v", m, err)
		}
	}
}

// TestRecordIdentity_AllFields checks every one of the 2^30 masks (every field combination, with
// and without empty sub-structs) under the first tricky values, decoding one in 4096. It takes
// minutes on every core, so it runs only with BIDE_RECORD_IDENTITY_ALL=1.
func TestRecordIdentity_AllFields(t *testing.T) {
	if os.Getenv("BIDE_RECORD_IDENTITY_ALL") != "1" {
		t.Skip("set BIDE_RECORD_IDENTITY_ALL=1 to check all 2^30 field combinations")
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
					for _, v := range trickyVals {
						r, l := idPair(uint32(m), v)
						if err := checkIdentity(r, l, m%4096 == 0); err != nil {
							err = fmt.Errorf("mask %#x: %w", m, err)
							failed.Store(&err)
							return
						}
					}
				}
				done.Add(1 << 16)
			}
		})
	}
	wg.Wait()
	if p := failed.Load(); p != nil {
		t.Fatal(*p)
	}
	t.Logf("%d masks x %d value sets checked", done.Load(), len(trickyVals))
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
		r, l := idPair(mask&(1<<idBits-1), v)
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
		view := legacyView(got)
		view.raw = nil
		if !reflect.DeepEqual(view, want) {
			t.Fatalf("decode %q:\n got %+v\nwant %+v", b, view, want)
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
			out = append(out, corpusMask{rng.Uint32() & (1<<idFields - 1), vi})
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
