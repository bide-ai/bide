package identitycmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

type env struct {
	t              *testing.T
	dir            string
	root, inter    *testca.CA
	rootK, interK  string
	srv            *httptest.Server
	mu             sync.Mutex
	routes         map[string][]byte
	policy, params string
	cache          string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), routes: map[string][]byte{}}
	e.root = testca.NewRoot(t, "root", testca.Opts{})
	e.inter = e.root.NewIntermediate(t, "inter", testca.Opts{})
	rk, ik := audit.SPKIHashOf(e.root.Cert), audit.SPKIHashOf(e.inter.Cert)
	e.rootK, e.interK = hex.EncodeToString(rk[:]), hex.EncodeToString(ik[:])
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		b, ok := e.routes[r.URL.Path]
		e.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(e.srv.Close)
	write := func(name string, b []byte) string {
		p := filepath.Join(e.dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("root.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.root.Cert.Raw}))
	write("inter.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.inter.Cert.Raw}))
	pol := map[string]any{
		"anchors": []string{"root.pem"}, "intermediates": []string{e.interK}, "intermediate_certs": []string{"inter.pem"},
		"identity": map[string]string{"alice": testca.Person(1)}, "id_kind": "person-uri",
		"eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(), "canonical": map[string]string{"host": "idp.bank.example"},
		"crl": map[string]any{
			e.rootK:  map[string]any{"urls": []string{e.srv.URL + "/root.crl"}},
			e.interK: map[string]any{"urls": []string{e.srv.URL + "/inter.crl"}},
		},
	}
	pb, _ := json.Marshal(pol)
	e.policy = write("policy.json", pb)
	par := map[string]any{
		"ca_period": "30m", "max_status_age": "1h", "exec_status_age": "1h",
		"issuers": map[string]any{
			e.rootK:  map[string]any{"ca_period": "24h", "max_status_age": "96h", "exec_status_age": "48h", "ca_overlap": "0s"},
			e.interK: map[string]any{"ca_period": "30m", "max_status_age": "1h", "exec_status_age": "1h", "ca_overlap": "0s"},
		},
	}
	qb, _ := json.Marshal(par)
	e.params = write("params.json", qb)
	e.cache = filepath.Join(e.dir, "cache")
	return e
}

func (e *env) publish(rootNum, interNum int64) {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routes["/root.crl"] = e.root.NewCRL(e.t, testca.CRLOpts{Number: rootNum, ThisUpdate: now.Add(-time.Hour), NextUpdate: now.Add(48 * time.Hour)})
	e.routes["/inter.crl"] = e.inter.NewCRL(e.t, testca.CRLOpts{Number: interNum, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour)})
}

func (e *env) common() []string {
	return []string{"-policy", e.policy, "-params", e.params, "-cache", e.cache}
}

func (e *env) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	st := run(context.Background(), args, &out, &errb)
	return st, out.String(), errb.String()
}

func TestFetchOnce(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1)
	st, out, errs := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...)
	if st != 0 {
		t.Fatalf("status %d: %s %s", st, out, errs)
	}
	events := map[string]int{}
	healths := 0
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("not a JSON line: %q", ln)
		}
		events[m["event"].(string)]++
		if m["event"] == "health" {
			healths++
			if m["critical"] != false {
				t.Fatalf("health %v", m)
			}
		}
	}
	if events["fetched"] != 2 || healths != 2 {
		t.Fatalf("events %v", events)
	}
	ents, err := os.ReadDir(filepath.Join(e.cache, "crl", e.interK))
	if err != nil || len(ents) < 2 {
		t.Fatalf("archive: %v %v", ents, err)
	}
}

func TestFetchOnceFailsWhenAnIssuerHasNoFreshCRL(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1)
	e.mu.Lock()
	delete(e.routes, "/inter.crl")
	e.mu.Unlock()
	st, _, errs := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...)
	if st != 1 || !strings.Contains(errs, "without a fresh CRL") {
		t.Fatalf("status %d: %s", st, errs)
	}
}

func TestUsageErrors(t *testing.T) {
	e := newEnv(t)
	for name, args := range map[string][]string{
		"no verb":          {},
		"unknown verb":     {"nope"},
		"no policy":        {"fetch-crl", "-params", e.params, "-cache", e.cache},
		"bad issuer":       append([]string{"reset-high-water", "-issuer", "zz", "-reason", "r", "-operator", "o"}, e.common()...),
		"ack without name": append([]string{"ack-reset", "-issuer", e.interK}, e.common()...),
		"stray argument":   append(append([]string{"fetch-crl"}, e.common()...), "extra"),
	} {
		if st, _, _ := e.run(args...); st != 2 {
			t.Errorf("%s: status %d, want 2", name, st)
		}
	}
	if st, _, _ := e.run("fetch-crl", "-h"); st != 0 {
		t.Errorf("-h: %d", st)
	}
}

func TestLDAPInThePolicyIsRefused(t *testing.T) {
	e := newEnv(t)
	b, _ := os.ReadFile(e.policy)
	b = bytes.Replace(b, []byte(e.srv.URL+"/root.crl"), []byte("ldap://ldap.bank.example/cn=root"), 1)
	if err := os.WriteFile(e.policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, _, errs := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...)
	if st != 1 || !strings.Contains(errs, "LDAP is not supported") {
		t.Fatalf("status %d: %s", st, errs)
	}
}

func TestResetHighWaterAndAck(t *testing.T) {
	e := newEnv(t)
	e.publish(1, 1_000_000)
	if st, _, errs := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...); st != 0 {
		t.Fatalf("%d %s", st, errs)
	}
	e.publish(1, 6)
	if st, _, _ := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...); st != 1 {
		t.Fatalf("a rollback below the poisoned mark passed: %d", st)
	}
	if st, _, _ := e.run(append([]string{"reset-high-water", "-issuer", e.interK, "-operator", "dayna"}, e.common()...)...); st != 1 {
		t.Fatalf("a reset without a reason: %d", st)
	}
	st, _, errs := e.run(append([]string{"reset-high-water", "-issuer", e.interK, "-reason", "CA numbering mistake", "-operator", "dayna"}, e.common()...)...)
	if st != 0 {
		t.Fatalf("reset: %d %s", st, errs)
	}
	if st, _, errs := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...); st != 0 {
		t.Fatalf("after the reset: %d %s", st, errs)
	}
	ents, _ := filepath.Glob(filepath.Join(e.cache, "crl", e.interK, "RESET-*.json"))
	if len(ents) != 1 {
		t.Fatalf("reset records %v", ents)
	}
	name := filepath.Base(ents[0])
	if st, out, _ := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...); st != 0 || !strings.Contains(out, "unacknowledged high-water reset") {
		t.Fatalf("the start did not report the unacknowledged reset (%d): %s", st, out)
	}
	if st, _, errs := e.run(append([]string{"ack-reset", "-issuer", e.interK, "-name", name}, e.common()...)...); st != 0 {
		t.Fatalf("ack: %d %s", st, errs)
	}
	if _, err := os.Stat(ents[0] + ".ack"); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := e.run(append([]string{"fetch-crl", "-once"}, e.common()...)...); strings.Contains(out, "unacknowledged high-water reset") {
		t.Fatal("an acknowledged reset is still reported")
	}
}
