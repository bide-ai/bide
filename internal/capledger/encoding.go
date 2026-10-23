package capledger

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The record encoding: every byte the ledger writes or reads is encoded and decoded here, and nowhere
// else, so a golden file (testdata/*.golden) pins the format of the four records: a slot, a cap-intent,
// a cap-reserve and an index entry. Field order is the struct's, and nothing in them is a map, so the
// bytes are a function of the values.

const (
	reserveFormat = "bide.cap-reserve.v1"
)

// reserveRecord is the audit record of a reservation, written in the run that made the call after the
// slot. Recovery does not read it.
type reserveRecord struct {
	Format string `json:"format"`
	Ledger string `json:"ledger"`
	Slot   int    `json:"slot"`
	Cum    int64  `json:"cum_after"`
}

// windowRecord is an index run entry, win:<window>.
type windowRecord struct {
	Window string `json:"window"`
}

func encodeSlot(s Slot) ([]byte, error)             { return json.Marshal(s) }
func encodeIntent(in intent) ([]byte, error)        { return json.Marshal(in) }
func encodeReserve(r reserveRecord) ([]byte, error) { return json.Marshal(r) }
func encodeWindow(w string) ([]byte, error)         { return json.Marshal(windowRecord{Window: w}) }

// strict decodes b into v, refusing a member v does not have and anything after the value: a record
// this package did not write does not decode.
func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("data after the record")
	}
	return nil
}

func decodeSlot(b []byte) (Slot, error) {
	var s Slot
	if err := strict(b, &s); err != nil {
		return Slot{}, fmt.Errorf("slot: %w", err)
	}
	return s, nil
}

func decodeIntent(b []byte) (intent, error) {
	var in intent
	if err := strict(b, &in); err != nil {
		return intent{}, fmt.Errorf("cap-intent: %w", err)
	}
	if in.Format != intentFormat {
		return intent{}, fmt.Errorf("cap-intent: format %q", in.Format)
	}
	return in, nil
}
