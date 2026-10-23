package capledger

import (
	"os"
	"strings"
	"testing"
)

// The golden file pins the bytes of the four records the ledger writes (a slot, a release slot, a
// cap-intent, a cap-reserve, an index entry): they are read by other processes and by older and newer
// versions of the code, so a change to them is deliberate. BIDE_UPDATE_GOLDEN=1 rewrites it.
func TestEncoding_Golden(t *testing.T) {
	reserve := Slot{N: 3, Kind: kindReserve, Attempt: "root/toolu_01/a1", Amount: 1250, Currency: "USD", CumAfter: 4100, Limit: 5000, Marked: true}
	over := Slot{N: 4, Kind: kindReserve, Attempt: "root/toolu_02/a1", Amount: 2000, Currency: "USD", CumAfter: 6100, Limit: 5000, Over: true}
	release := Slot{N: 5, Kind: kindRelease, Attempt: "root/toolu_01/a1", Amount: 1250, Currency: "USD", Ref: 3, CumAfter: 4850, Limit: 5000, Marked: true}
	in := intent{Format: intentFormat, Attempt: "root/toolu_01/a1", Namespace: "prod", Rule: "refund_total", Currency: "USD", Kind: Day,
		Key: KeyDigest("customer-17"), Amount: 1250, Marked: true, Window: "2026-10-22", Floor: "2026-10-21"}
	var b strings.Builder
	add := func(name string, data []byte, err error) {
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(name + " " + string(data) + "\n")
	}
	d, err := encodeSlot(reserve)
	add("slot-reserve", d, err)
	d, err = encodeSlot(over)
	add("slot-reserve-waived", d, err)
	d, err = encodeSlot(release)
	add("slot-release", d, err)
	d, err = encodeIntent(in)
	add("cap-intent", d, err)
	d, err = encodeReserve(reserveRecord{Format: reserveFormat, Ledger: "@cap/prod/refund_total/USD/2026-10-22/x", Slot: 3, Cum: 4100})
	add("cap-reserve", d, err)
	d, err = encodeWindow("2026-10-22")
	add("win", d, err)
	const path = "testdata/records.golden"
	if os.Getenv("BIDE_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != b.String() {
		t.Fatalf("the record encoding changed:\n got:\n%s\nwant:\n%s", b.String(), want)
	}
	// What the golden holds decodes to the same values.
	s, err := decodeSlot([]byte(strings.SplitN(strings.Split(string(want), "\n")[2], " ", 2)[1]))
	if err != nil || s != release {
		t.Fatalf("decoded release = %+v, %v", s, err)
	}
	got, err := decodeIntent([]byte(strings.SplitN(strings.Split(string(want), "\n")[3], " ", 2)[1]))
	if err != nil || got != in {
		t.Fatalf("decoded intent = %+v, %v", got, err)
	}
}
