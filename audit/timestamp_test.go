package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// A signed tree head's Timestamp is Unix nanoseconds. The rule EvidencePackage.Verify applies to
// every head a package carries: positive, not later than the verifier's clock plus the allowed
// skew, and, where one head extends another, not earlier than the head it extends.

// tsEvidence builds a sealed package over three tool calls in run A at timestamp ts, with the
// options given, and returns it with its store and keys.
func tsEvidence(t *testing.T, ts int64, opts ...EvidenceOption) (EvidencePackage, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	pkg, err := Evidence(context.Background(), s, "A", edS(priv), ts, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return pkg, pub, priv
}

func tsOK(t *testing.T, pkg EvidencePackage, pub ed25519.PublicKey) bool {
	t.Helper()
	rep, err := pkg.Verify(edV(pub), WithApprovedPolicies())
	if err := reportErr(rep.OK, err); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep.OK
}

func TestEvidence_HeadTimestampMustBePositiveAndNotInTheFuture(t *testing.T) {
	now := time.Now()
	for name, ts := range map[string]int64{
		"zero":                  0,
		"negative":              -5,
		"an hour in the future": now.Add(time.Hour).UnixNano(),
		"a year in the future":  now.AddDate(1, 0, 0).UnixNano(),
	} {
		pkg, pub, _ := tsEvidence(t, ts)
		if tsOK(t, pkg, pub) {
			t.Errorf("a package whose signed head has timestamp %s (%d) verified", name, ts)
		}
	}
	for name, ts := range map[string]int64{
		"an hour ago":                  now.Add(-time.Hour).UnixNano(),
		"a minute ahead (within skew)": now.Add(time.Minute).UnixNano(),
		"1":                            1,
	} {
		pkg, pub, _ := tsEvidence(t, ts)
		if !tsOK(t, pkg, pub) {
			t.Errorf("a package whose signed head has timestamp %s (%d) did not verify", name, ts)
		}
	}
}

// An action's bundle may carry its own signature over the same tree (SameTree ignores the
// timestamp); a future timestamp there fails the item.
func TestEvidence_ActionHeadTimestampIsChecked(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixNano()
	pkg, pub, priv := tsEvidence(t, past)
	th := pkg.Actions[0].Bundle.STH.TreeHead
	th.TimestampNanos = time.Now().Add(time.Hour).UnixNano()
	pkg.Actions[0].Bundle.STH = signTH(t, th, priv)
	if err := pkg.Seal(edS(priv)); err != nil {
		t.Fatal(err)
	}
	if tsOK(t, pkg, pub) {
		t.Fatal("a package whose action bundle is signed at a future time verified")
	}
}

// The consistency proof's earlier head must not be later than the head it extends.
func TestEvidence_ConsistencyHeadsAreInOrder(t *testing.T) {
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	recs, _ := s.History(context.Background(), "A")
	at := time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		from int64
		ok   bool
	}{
		"earlier":    {at.Add(-time.Minute).UnixNano(), true},
		"same time":  {at.UnixNano(), true},
		"later":      {at.Add(time.Minute).UnixNano(), false},
		"zero":       {0, false},
		"the future": {time.Now().Add(time.Hour).UnixNano(), false},
	} {
		early, err := journalHead("A", recs[:1], tc.from)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := Evidence(context.Background(), s, "A", edS(priv), at.UnixNano(), WithConsistencyFrom(signTH(t, early, priv)))
		if err != nil {
			t.Fatal(err)
		}
		if got := tsOK(t, pkg, pub); got != tc.ok {
			t.Errorf("consistency from a head signed %s: verified = %v, want %v", name, got, tc.ok)
		}
	}
}

// A run certificate's heads follow the rule too, and its used-policy head, projected from the
// journal head, is not earlier than it.
func TestEvidence_RunCertificateHeadTimestampsAreChecked(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		used int64
		ok   bool
	}{
		"same time":  {at.UnixNano(), true},
		"later":      {at.Add(time.Minute).UnixNano(), true},
		"earlier":    {at.Add(-time.Minute).UnixNano(), false},
		"the future": {time.Now().Add(time.Hour).UnixNano(), false},
	} {
		pub, priv := secKey(t)
		s := secThreeCalls(t, "A")
		pkg, err := Evidence(context.Background(), s, "A", edS(priv), at.UnixNano())
		if err != nil {
			t.Fatal(err)
		}
		cert, err := CertifyRun(context.Background(), s, "A", pkg.STH, RunCertSpec{Signer: edS(priv), TimestampNanos: at.UnixNano()})
		if err != nil {
			t.Fatal(err)
		}
		// CertifyRun refuses an earlier used-policy head, so re-sign it at tc.used by hand: the
		// verifier must refuse one that some other producer made.
		used := cert.UsedPolicyAbsence.TreeHead
		used.TimestampNanos = tc.used
		cert.UsedPolicyAbsence = signTH(t, used, priv)
		pkg.RunCertificate = &cert
		if err := pkg.Seal(edS(priv)); err != nil {
			t.Fatal(err)
		}
		if got := tsOK(t, pkg, pub); got != tc.ok {
			t.Errorf("a run certificate whose used-policy head is signed %s: verified = %v, want %v", name, got, tc.ok)
		}
	}
}

// The verifier's clock and skew are the caller's to set.
func TestEvidence_VerifyTimeAndSkewOptions(t *testing.T) {
	signed := time.Now().Add(-time.Hour)
	pkg, pub, _ := tsEvidence(t, signed.UnixNano())
	check := func(what string, want bool, opts ...EvidenceVerifyOption) {
		t.Helper()
		rep, err := pkg.Verify(edV(pub), append([]EvidenceVerifyOption{WithApprovedPolicies()}, opts...)...)
		if err := reportErr(rep.OK, err); err != nil {
			t.Fatal(err)
		}
		if rep.OK != want {
			t.Errorf("%s: verified = %v, want %v (%v)", what, rep.OK, want, rep.Problems)
		}
	}
	check("default clock", true)
	check("a clock a day before the signing", false, WithVerifyTime(signed.Add(-24*time.Hour)))
	check("a clock just past the skew before the signing", false, WithVerifyTime(signed.Add(-DefaultClockSkew-time.Second)))
	check("a clock just within the skew before the signing", true, WithVerifyTime(signed.Add(-DefaultClockSkew+time.Second)))
	check("a clock at the skew exactly", true, WithVerifyTime(signed.Add(-DefaultClockSkew)))
	check("a two-hour skew at a clock an hour before the signing", true, WithVerifyTime(signed.Add(-time.Hour)), WithClockSkew(2*time.Hour))
	check("no skew at a clock a nanosecond before the signing", false, WithVerifyTime(signed.Add(-time.Nanosecond)), WithClockSkew(0))
	check("no skew at the signing time", true, WithVerifyTime(signed), WithClockSkew(0))
	check("a negative skew", false, WithClockSkew(-time.Second))
}

func TestCheckTimestamp(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		ts   int64
		skew time.Duration
		ok   bool
	}{
		{1, 0, true},
		{0, time.Hour, false},
		{-1, time.Hour, false},
		{now.UnixNano(), 0, true},
		{now.UnixNano() + 1, 0, false},
		{now.UnixNano() + int64(time.Second), time.Second, true},
		{now.UnixNano() + int64(time.Second) + 1, time.Second, false},
		{1, -1, false},
	} {
		err := CheckTimestamp(TreeHead{Kind: TreeJournal, RunID: "r", TimestampNanos: tc.ts}, now, tc.skew)
		if (err == nil) != tc.ok {
			t.Errorf("CheckTimestamp(ts %d, skew %s) = %v, want ok %v", tc.ts, tc.skew, err, tc.ok)
		}
	}
	if CheckTimestampOrder(TreeHead{TimestampNanos: 5}, TreeHead{TimestampNanos: 5}) != nil ||
		CheckTimestampOrder(TreeHead{TimestampNanos: 5}, TreeHead{TimestampNanos: 6}) != nil ||
		CheckTimestampOrder(TreeHead{TimestampNanos: 6}, TreeHead{TimestampNanos: 5}) == nil {
		t.Error("CheckTimestampOrder: a later head must not be earlier than the head it extends, and may equal it")
	}
}

// The heads of a run certificate's convergence evidence follow the rule as well.
func TestEvidence_RunCertificateConvergenceHeadsAreChecked(t *testing.T) {
	ctx := context.Background()
	for _, which := range []string{"none", "policy leaf", "certificate leaf"} {
		pub, priv := secKey(t)
		s := secThreeCalls(t, "A")
		if _, err := RecordPolicy(ctx, s, "A", []byte("policy"), "D"); err != nil {
			t.Fatal(err)
		}
		if _, err := RecordConvergence(ctx, s, "A", []byte(`{}`), "D"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Do(ctx, "A", "governed", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepToolResult, ToolUseID: "g", Result: json.RawMessage(`{"policy_digest":"D"}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-time.Hour).UnixNano()
		pkg, err := Evidence(ctx, s, "A", edS(priv), at, WithRunCertificate(RunCertSpec{ApprovedPolicies: []string{"D"}}))
		if err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour).UnixNano()
		resign := func(b *ProofBundle) {
			th := b.STH.TreeHead
			th.TimestampNanos = future
			b.STH = signTH(t, th, priv)
		}
		switch which {
		case "policy leaf":
			resign(&pkg.RunCertificate.Convergence[0].PolicyLeaf)
		case "certificate leaf":
			resign(&pkg.RunCertificate.Convergence[0].Certificate)
		}
		if err := pkg.Seal(edS(priv)); err != nil {
			t.Fatal(err)
		}
		rep, err := pkg.Verify(edV(pub), WithApprovedPolicies("D"))
		if err := reportErr(rep.OK, err); err != nil {
			t.Fatal(err)
		}
		if want := which == "none"; rep.OK != want {
			t.Errorf("run certificate with a future-signed %s head: verified = %v, want %v (%+v)", which, rep.OK, want, rep)
		}
	}
}

// The package's own head is checked even when every bundle it carries has a good one.
func TestEvidence_PackageHeadIsCheckedOnItsOwn(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixNano()
	pkg, pub, priv := tsEvidence(t, past)
	th := pkg.STH.TreeHead
	th.TimestampNanos = time.Now().Add(time.Hour).UnixNano()
	pkg.STH = signTH(t, th, priv)
	if err := pkg.Seal(edS(priv)); err != nil {
		t.Fatal(err)
	}
	if tsOK(t, pkg, pub) {
		t.Fatal("a package whose own head is signed in the future verified")
	}
}

// A run certificate's journal head is checked on its own too: here it carries timestamp 0 while
// the package's head and the certificate's used-policy head are good.
func TestEvidence_RunCertificateJournalHeadIsCheckedOnItsOwn(t *testing.T) {
	at := time.Now().Add(-time.Hour).UnixNano()
	pub, priv := secKey(t)
	s := secThreeCalls(t, "A")
	pkg, err := Evidence(context.Background(), s, "A", edS(priv), at)
	if err != nil {
		t.Fatal(err)
	}
	th := pkg.STH.TreeHead
	th.TimestampNanos = 0
	zero := signTH(t, th, priv)
	cert, err := CertifyRun(context.Background(), s, "A", zero, RunCertSpec{Signer: edS(priv), TimestampNanos: at})
	if err != nil {
		t.Fatal(err)
	}
	pkg.RunCertificate = &cert
	if err := pkg.Seal(edS(priv)); err != nil {
		t.Fatal(err)
	}
	if tsOK(t, pkg, pub) {
		t.Fatal("a run certificate whose journal head has timestamp 0 verified")
	}
}
