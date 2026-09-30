package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// formatFixtures returns one genuine instance of each formatted artifact, produced by its producer.
func formatFixtures(t *testing.T) map[string]formatted {
	t.Helper()
	ctx := context.Background()
	store, recs, sth := fuzzRun(t)
	pb, err := ProveRecord(ctx, store, "run", 1, sth)
	if err != nil {
		t.Fatal(err)
	}
	head, err := NewAbsenceTreeHead(recs, ToolUseKeys, sth.TreeHead, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ab, err := ProveAbsentBundle(recs, ToolUseKeys, "tooluse:zz", SignTreeHead(head, fuzzPriv))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Evidence(ctx, store, "run", fuzzPriv, 1000, WithAllToolCalls(), WithConsistencyFrom(sth))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := CertifyRun(ctx, store, "run", sth, RunCertSpec{}, fuzzPriv, 1000)
	if err != nil {
		t.Fatal(err)
	}
	cg, err := ProveCurrentGrant(ctx, store, "run", sth, 1)
	if err != nil {
		t.Fatal(err)
	}
	log, err := EventLogFromJournal(ctx, store, "run")
	if err != nil {
		t.Fatal(err)
	}
	ei, err := log.Prove(0)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]formatted{
		"ProofBundle": pb, "AbsenceBundle": ab, "EvidencePackage": ev,
		"RunCertificate": rc, "CurrentGrantProof": cg, "EventInclusion": ei,
	}
}

// Each producer stamps its artifact with the format this version reads, and the artifact's JSON
// carries it as "format", with the proof fields in snake_case.
func TestArtifactsCarryTheirFormat(t *testing.T) {
	for name, a := range formatFixtures(t) {
		_, want := a.artifactFormat()
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(`"format":"`+want+`"`)) {
			t.Errorf("%s JSON does not carry format %q: %s", name, want, b)
		}
		for _, goName := range []string{`"Index"`, `"Size"`, `"Path"`, `"First"`, `"Salt"`} {
			if bytes.Contains(b, []byte(goName)) {
				t.Errorf("%s JSON has the Go field name %s: %s", name, goName, b)
			}
		}
		// It decodes strictly as it was written.
		v := reflect.New(reflect.TypeOf(a))
		if err := UnmarshalStrict(b, v.Interface()); err != nil {
			t.Errorf("%s does not decode: %v", name, err)
		}
	}
}

// oldLayout rewrites an artifact's JSON to the layout before formats: no "format" member at any
// depth, and the inclusion and consistency fields in Go case.
func oldLayout(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		panic(err)
	}
	var walk func(any) any
	walk = func(x any) any {
		switch x := x.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, e := range x {
				switch k {
				case "format":
					continue
				case "index", "path", "first", "salt":
					k = strings.ToUpper(k[:1]) + k[1:]
				case "size":
					if _, inProof := x["path"]; inProof {
						k = "Size"
					}
				}
				out[k] = walk(e)
			}
			return out
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		}
		return x
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		panic(err)
	}
	return out
}

// An artifact in the layout before formats, or of another format, fails to decode with ErrFormat
// and a message naming the format, not with an unknown-field error about "Index".
func TestUnmarshalStrictRejectsOtherFormats(t *testing.T) {
	for name, a := range formatFixtures(t) {
		kind, want := a.artifactFormat()
		b, _ := json.Marshal(a)
		v := reflect.New(reflect.TypeOf(a)).Interface()

		err := UnmarshalStrict(oldLayout(b), v)
		if !errors.Is(err, ErrFormat) || !strings.Contains(err.Error(), "the "+kind+" has no format") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s in the old layout: err = %v, want ErrFormat naming %q", name, err, want)
		}

		other := bytes.Replace(b, []byte(`"format":"`+want+`"`), []byte(`"format":"bide.audit.other.v9"`), 1)
		err = UnmarshalStrict(other, v)
		if !errors.Is(err, ErrFormat) || !strings.Contains(err.Error(), `"bide.audit.other.v9"`) {
			t.Errorf("%s of another format: err = %v, want ErrFormat naming the format", name, err)
		}

		notString := bytes.Replace(b, []byte(`"format":"`+want+`"`), []byte(`"format":2`), 1)
		if err := UnmarshalStrict(notString, v); !errors.Is(err, ErrFormat) {
			t.Errorf("%s with a numeric format: err = %v, want ErrFormat", name, err)
		}
	}
}

// Verification refuses an artifact of another format with ErrFormat, whether or not it came
// through UnmarshalStrict (encoding/json matches "Index" to the new names case-insensitively, so
// a leniently decoded old artifact otherwise looks current).
func TestVerifyRejectsOtherFormats(t *testing.T) {
	fx := formatFixtures(t)
	ctx := context.Background()

	pb := fx["ProofBundle"].(ProofBundle)
	if ok, err := pb.Verify(fuzzPub); !ok || err != nil {
		t.Fatalf("genuine ProofBundle: %v, %v", ok, err)
	}
	pb.Format = ""
	if ok, err := pb.Verify(fuzzPub); ok || !errors.Is(err, ErrFormat) {
		t.Errorf("ProofBundle without a format: %v, %v; want false, ErrFormat", ok, err)
	}

	ab := fx["AbsenceBundle"].(AbsenceBundle)
	if ok, err := ab.Verify(fuzzPub, ToolUseKeys); !ok || err != nil {
		t.Fatalf("genuine AbsenceBundle: %v, %v", ok, err)
	}
	ab.Format = "bide.audit.absence.v1"
	if ok, err := ab.Verify(fuzzPub, ToolUseKeys); ok || !errors.Is(err, ErrFormat) {
		t.Errorf("AbsenceBundle of another format: %v, %v; want false, ErrFormat", ok, err)
	}

	rc := fx["RunCertificate"].(RunCertificate)
	if res, err := VerifyRun(rc, nil, fuzzPub); !res.OK || err != nil {
		t.Fatalf("genuine RunCertificate: %+v, %v", res, err)
	}
	rc.Format = ""
	if res, err := VerifyRun(rc, nil, fuzzPub); res.OK || !errors.Is(err, ErrFormat) {
		t.Errorf("RunCertificate without a format: %+v, %v; want not OK, ErrFormat", res, err)
	}

	cg := fx["CurrentGrantProof"].(CurrentGrantProof)
	cg.Format = ""
	if ok, err := VerifyCurrentGrant(SignedGrant{}, "run", cg, nil, fuzzPub); ok || !errors.Is(err, ErrFormat) {
		t.Errorf("CurrentGrantProof without a format: %v, %v; want false, ErrFormat", ok, err)
	}
	cg = fx["CurrentGrantProof"].(CurrentGrantProof)
	cg.Leaf.Format = ""
	if ok, err := VerifyCurrentGrant(SignedGrant{}, "run", cg, nil, fuzzPub); ok || !errors.Is(err, ErrFormat) {
		t.Errorf("CurrentGrantProof whose leaf has no format: %v, %v; want false, ErrFormat", ok, err)
	}

	store, _, _ := fuzzRun(t)
	log, err := EventLogFromJournal(ctx, store, "run")
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := projectJournal(ctx, store, "run")
	if err != nil {
		t.Fatal(err)
	}
	ei, err := log.Prove(0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyEventInclusion(log.Root(), events[0], ei); !ok || err != nil {
		t.Fatalf("genuine EventInclusion: %v, %v", ok, err)
	}
	ei.Format = ""
	if ok, err := VerifyEventInclusion(log.Root(), events[0], ei); ok || !errors.Is(err, ErrFormat) {
		t.Errorf("EventInclusion without a format: %v, %v; want false, ErrFormat", ok, err)
	}
}

// A format names exactly one layout, so its name is pinned: a layout change must choose a new
// name here, never reuse one a verifier in the field already reads as something else.
func TestFormatNames(t *testing.T) {
	for got, want := range map[string]string{
		ProofFormat:          "bide.audit.proof.v2",
		AbsenceFormat:        "bide.audit.absence.v2",
		RunCertificateFormat: "bide.audit.runcert.v2",
		CurrentGrantFormat:   "bide.audit.current-grant.v2",
		EventInclusionFormat: "bide.audit.event-inclusion.v2",
		EvidenceFormat:       "bide.audit.evidence.v4",
	} {
		if got != want {
			t.Errorf("format %q, want %q", got, want)
		}
	}
}
