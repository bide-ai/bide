package agent

import (
	"context"
	"reflect"
	"testing"
)

// A decoded record carries a ModelTurn / ApproverSignature although its journal encoding holds none,
// when the bytes name a moved member with an empty value. The PR says a decoded record carries one
// exactly when its encoding does.
func TestR140_DecodedEmptySubStruct(t *testing.T) {
	for _, in := range []string{
		`{"name":"a","kind":"model","finish":""}`,
		`{"name":"a","kind":"model","model":null}`,
		`{"name":"a","kind":"approval","approver":""}`,
		`{"name":"a","kind":"approval","signature":null}`,
	} {
		r, err := DecodeRecord([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		enc, _ := EncodeRecord(r)
		back, _ := DecodeRecord(enc)
		back.raw, r.raw = nil, nil
		if !reflect.DeepEqual(r, back) {
			t.Errorf("%s: decoded ModelTurn=%v ApproverSignature=%v, but its encoding %s decodes to ModelTurn=%v ApproverSignature=%v",
				in, r.ModelTurn, r.ApproverSignature, enc, back.ModelTurn, back.ApproverSignature)
		}
	}
}

// Do and History hand back records that share no sub-struct with the record fn returned or with
// each other, so a caller replacing nothing but writing through the pointer cannot alter the journal.
func TestR140_StoreCopiesSubStructs(t *testing.T) {
	ctx := context.Background()
	m := memJournal()
	built := Record{Kind: StepModel, ModelTurn: &ModelTurn{Finish: "stop"}}
	live, err := m.do(ctx, "r", "s", func(context.Context) (Record, error) { return built, nil })
	if err != nil {
		t.Fatal(err)
	}
	if live.ModelTurn == built.ModelTurn {
		t.Fatal("Do returned the built ModelTurn pointer")
	}
	live.ModelTurn.Finish = "X"
	h, _ := m.History(ctx, "r")
	if h[len(h)-1].Finish() != "stop" {
		t.Fatalf("History reads %q after a caller wrote through Do's ModelTurn", h[len(h)-1].Finish())
	}
	h[len(h)-1].ModelTurn.Finish = "Y"
	h2, _ := m.History(ctx, "r")
	memo, _ := m.do(ctx, "r", "s", func(context.Context) (Record, error) { t.Fatal("ran"); return Record{}, nil })
	if h2[len(h2)-1].Finish() != "stop" || memo.Finish() != "stop" || built.Finish() != "stop" {
		t.Fatalf("aliasing: history %q memo %q built %q", h2[len(h2)-1].Finish(), memo.Finish(), built.Finish())
	}
}
