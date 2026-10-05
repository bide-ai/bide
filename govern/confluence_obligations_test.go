package govern_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// buildObligations returns a machine that converges with all three obligations gsm v0.13.0
// reports: "inc" saturates a capped counter and is not idempotent, and "open"/"close" are an
// undeclared pair that does not commute (only the pairs with "inc" are declared Independent), so
// convergence needs them delivered in causal order.
func buildObligations(t *testing.T) (*gsm.Report, string) {
	t.Helper()
	r := gsm.NewRegistry("obligations")
	n := r.Int("n", 0, 2)
	open := r.Bool("open")
	r.On("inc").Does(gsm.Inc(n)).Add()
	r.On("open").Does(gsm.SetTo(open, 1)).Add()
	r.On("close").Does(gsm.SetTo(open, 0)).Add()
	r.Independent("inc", "open").Independent("inc", "close")
	_, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, rep)
	}
	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return rep, digest
}

// The certificate carries the delivery obligations gsm's Report names, so a certificate does not
// read as unconditional when convergence needs causal delivery or deduplication.
func TestCertifyConvergence_CarriesObligations(t *testing.T) {
	rep, digest := buildObligations(t)
	if len(rep.CausalOrderRequired) == 0 || len(rep.NotIdempotent) == 0 || len(rep.Saturations) == 0 {
		t.Fatalf("the fixture must exercise every obligation; gsm reported:\n%s", rep)
	}
	c := govern.CertifyConvergence(rep, digest)
	if !c.Converges {
		t.Fatalf("Build returned a machine, so the certificate converges: %+v", c)
	}
	if c.PairsUndeclared != rep.PairsUndeclared || c.PairsUndeclared == 0 {
		t.Errorf("PairsUndeclared = %d, gsm reported %d", c.PairsUndeclared, rep.PairsUndeclared)
	}
	if want := []govern.EventPair{{First: "open", Second: "close"}}; !samePairs(c.CausalOrderRequired, want) {
		t.Errorf("CausalOrderRequired = %v, want %v", c.CausalOrderRequired, want)
	}
	if !reflect.DeepEqual(c.NotIdempotent, rep.NotIdempotent) || !contains(c.NotIdempotent, "inc") {
		t.Errorf("NotIdempotent = %v, gsm reported %v", c.NotIdempotent, rep.NotIdempotent)
	}
	if len(c.Saturations) != len(rep.Saturations) {
		t.Fatalf("Saturations = %v, gsm reported %v", c.Saturations, rep.Saturations)
	}
	for i, s := range rep.Saturations {
		if got := c.Saturations[i]; got.Rule != s.Rule || got.Var != s.Var || got.States != s.States {
			t.Errorf("Saturations[%d] = %+v, gsm reported %+v", i, got, s)
		}
	}

	out := c.String()
	for _, want := range []string{
		"Convergence: GUARANTEED under causal delivery of the 1 undeclared pair(s) below",
		"causal order required for 1 undeclared pair(s): ",
		"exactly once: inc",
		"ApplyOnce",
		"saturation:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("String() does not mention %q:\n%s", want, out)
		}
	}
}

// A certificate with no obligations records each list as empty, not absent, so a reader can tell
// "checked, none" from a certificate written before the lists existed.
func TestCertifyConvergence_EmptyObligationsArePresent(t *testing.T) {
	rep, digest := buildGoverned(t)
	b, err := govern.CertifyConvergence(rep, digest).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"causal_order_required", "not_idempotent", "saturations"} {
		if got := string(keys[k]); got != "[]" {
			t.Errorf("%s = %s, want []", k, got)
		}
	}
	if got := string(keys["pairs_undeclared"]); got != "0" {
		t.Errorf("pairs_undeclared = %s, want 0", got)
	}
	if out := govern.CertifyConvergence(rep, digest).String(); strings.Contains(out, "causal order") || strings.Contains(out, "exactly once") {
		t.Errorf("a certificate with no obligations names one:\n%s", out)
	}
}

func samePairs(got, want []govern.EventPair) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		g, w := got[i], want[i]
		if g != w && (g.First != w.Second || g.Second != w.First) {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
