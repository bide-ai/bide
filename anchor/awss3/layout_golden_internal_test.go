package awss3

import "testing"

// Pinned before the layout code moved to audit/anchorobj; see TestObjectLayoutIsByteIdentical.
func TestPartitionAndSizeNamesAreByteIdentical(t *testing.T) {
	for _, c := range []struct{ kind, run, want string }{
		{"journal", "run-1", "08b41df3b7d966b1d62a971e7b5c3c935bdda6fc5588147e435ff58c8ae7878f"},
		{"events", "run-1", "6abd871619456aeebf7b98bacf3a95e7e589d6902bc5e99956efdd9e49ae1b52"},
		{"events", "notify:@open:2026-10-17", "0fa5f21240dbdf75a089a57889de765e9162b2d1a2196ef64754956831b739c5"},
		{"journal", "", "728b8bd3c1a9ea2a73b8dc0b3b15c4bd6b6979253dc2445b245b3df6b2d8785f"},
	} {
		if got := partitionOf(c.kind, c.run); got != c.want {
			t.Errorf("partitionOf(%q, %q) = %s, want %s", c.kind, c.run, got, c.want)
		}
	}
	for size, want := range map[int]string{0: "9223372036854775807", 1: "9223372036854775806", 123456: "9223372036854652351"} {
		if got := invertedSize(size); got != want {
			t.Errorf("invertedSize(%d) = %s, want %s", size, got, want)
		}
	}
}
