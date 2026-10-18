package anchorobj_test

import (
	"testing"

	"github.com/bide-ai/bide/audit/anchorobj"
)

func TestSuspectID(t *testing.T) {
	for id, want := range map[string]bool{
		"jane@example.com": true, "refund:jane.doe@bank.co.uk": true, "acct-123456789012": true, "4111111111111111": true,
		"jane.doe@openbank.com": true, "jane@openmail.example.org": true, "4111-1111-1111-1111": true, "4111 1111 1111 1111": true,
		"acct:@jane.doe.com": true, "jane.doe@localhost": true, "notify:@open:jane@x.com": true, "refund/jane@x.io/7": true,
		"4111-1111-1111-1": true, "4111-1111-1111-": false, "4111 1111 1111": false, "4111-1111-1111": false, "411111111111": true, "41111111111": false,
		"jane@LocalHost": true, "a@.com": false, "a@host.": false, "a@b.c": true, "@a.b": false, "x notify:@open:2026": false,
		"notify:@open:411111111111": true,
		"run-1":                     false, "notify:@open:2026-10-17": false, "notify:@open:index:2026": false, "2026-10-17T12:00:00Z-0001": false,
		"run-2026-10-17-0001": false, "9b2e7c1a-5d34-4f10-8a6e-0c1d2e3f4a5b": false, "@handle": false, "user@host": false, "wf/refund/7": false,
	} {
		if got := anchorobj.SuspectID(id) != ""; got != want {
			t.Errorf("anchorobj.SuspectID(%q) = %v, want %v", id, got, want)
		}
	}
}
