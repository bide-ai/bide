package ssocerts

// The SSO short-lived certificate recipe, end to end, against a real step-ca (0.30.2) and the real step
// CLI (0.31.0) in containers, and a mock OIDC identity provider (mock-oauth2-server). The images are
// pinned by digest. The test needs Docker; BIDE_REQUIRE_STEPCA=1 makes a missing Docker or image a
// failure, not a skip (the Integration job sets it, and fails on any skip).
//
// What runs, in order: the bank's two-tier PKI (root, and an issuing CA with a policy OID, a pathlen of
// 0 and a name constraint) is made; the IdP and step-ca start; an approver makes a key and a request
// with bide keygen -csr; scripts/renew.sh has step ca sign it through the OIDC provisioner (the real
// login, with a stand-in for the browser); the certificate is judged by the same rules the gate applies;
// logins that did not use MFA, or whose person id is malformed, get no certificate.

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// The images, by digest: step-ca 0.30.2, step CLI 0.31.0, mock-oauth2-server 2.1.10.
const (
	imgCA  = "smallstep/step-ca@sha256:a2b17872915c193259b75a5474c398326f41bd199f0842093e52cf4182bc8270"
	imgCLI = "smallstep/step-cli@sha256:107e8497646c0e7c06cc5b6fd6fa13b5d2001909df6d7da3706b8541ed42bddf"
	imgIdP = "ghcr.io/navikt/mock-oauth2-server@sha256:65d4ed47ce094cc3849e41e03499bfb32372d69c726ec22b3d5a19eba9bc787e"

	crlURL = "http://localhost:19001/1.0/crl"
)

// rig is the running recipe: the directories, the containers, the binaries and the CA's facts.
type rig struct {
	t               *testing.T
	dir             string // the work directory, mounted into the step CLI container at the same path
	pki             string // the CA container's /pki
	idpName, caName string
	bide, bideHSM   string
	step            string // the wrapper that runs the step CLI in its container
	root, inter     *testca.CA
	idpPEM          string
	policy, params  string
}

func requireDocker(t *testing.T) {
	t.Helper()
	fail := func(format string, args ...any) {
		if os.Getenv("BIDE_REQUIRE_STEPCA") != "" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	// The test pulls three images and runs for about a minute, so it runs only when asked to: by
	// BIDE_TEST_STEPCA=1, or by BIDE_REQUIRE_STEPCA=1 (the Integration job), which also turns a
	// missing Docker into a failure.
	if os.Getenv("BIDE_TEST_STEPCA") == "" && os.Getenv("BIDE_REQUIRE_STEPCA") == "" {
		t.Skip("set BIDE_TEST_STEPCA=1 to run the SSO recipe against containers (needs Docker)")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		fail("the recipe test runs where Docker can mount host directories at the same path (linux, macOS)")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		fail("Docker is not available: %v %s", err, out)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func tryRun(env []string, dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func render(t *testing.T, src, dst string, data any) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New(filepath.Base(src)).Delims("[[", "]]").Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, data); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("%s did not render to JSON:\n%s", src, out.String())
	}
	if err := os.WriteFile(dst, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePEM(t *testing.T, path, typ string, der []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
		t.Fatal(err)
	}
}

// build compiles a command of the cmd/bide module into dir.
func build(t *testing.T, dir, module, out string) string {
	t.Helper()
	exe := filepath.Join(dir, out)
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Dir = module
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", module, err, b)
	}
	return exe
}

type provisioner struct {
	Name, ClientID string
	Last           bool
}

type client struct {
	ClientID, Email, PersonID, AMR string
	Last                           bool
}

// The three logins the IdP knows: a person who used MFA, one who did not, and one whose person id is
// not what the template accepts. The CA has one OIDC provisioner per client id.
var (
	provisioners = []provisioner{{Name: "sso", ClientID: "step-client"}, {Name: "sso-nomfa", ClientID: "step-client-nomfa"}, {Name: "sso-badid", ClientID: "step-client-badid", Last: true}}
	clients      = []client{
		{ClientID: "step-client", Email: "ops@bank.example", PersonID: "00000000000000000000000000000001", AMR: `["pwd","mfa"]`},
		{ClientID: "step-client-nomfa", Email: "ops@bank.example", PersonID: "00000000000000000000000000000001", AMR: `["pwd"]`},
		{ClientID: "step-client-badid", Email: "ops@bank.example", PersonID: "OPS-1", AMR: `["pwd","mfa"]`, Last: true},
	}
)

func newRig(t *testing.T) *rig {
	t.Helper()
	requireDocker(t)
	r := &rig{t: t, dir: t.TempDir()}
	// macOS: a temp directory under /var is a symlink to /private/var, which Docker does not mount by the short name.
	if d, err := filepath.EvalSymlinks(r.dir); err == nil {
		r.dir = d
	}
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	r.pki = filepath.Join(r.dir, "pki")
	for _, d := range []string{"certs", "secrets", "templates", "config", "db", "work"} {
		if err := os.MkdirAll(filepath.Join(r.pki, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(r.pki, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"certs", "secrets", "templates", "config", "db", "work"} {
		_ = os.Chmod(filepath.Join(r.pki, d), 0o777)
	}
	r.bide = build(t, r.dir, filepath.Join(here, "../../cmd/bide"), "bide")
	r.bideHSM = build(t, r.dir, filepath.Join(here, "../../cmd/bide/bide-hsm"), "bide-hsm")

	// The bank's PKI. The root is offline in a bank; here its key is in memory. The issuing CA carries the
	// certificate policy the leaves assert, a pathlen of 0 and a name constraint on the person URI host.
	now := time.Now().UTC().Truncate(time.Second)
	valid := func(c *x509.Certificate) { c.NotBefore, c.NotAfter = now.Add(-time.Hour), now.Add(365*24*time.Hour) }
	r.root = testca.NewRoot(t, "Example Bank Approver Root", testca.Opts{Mutate: valid})
	r.inter = r.root.NewIntermediate(t, "Example Bank Approver Issuing CA", testca.Opts{Mutate: func(c *x509.Certificate) {
		valid(c)
		c.MaxPathLen, c.MaxPathLenZero = 0, true
		c.Policies = []x509.OID{testca.PolicyOID}
		c.PermittedURIDomains, c.PermittedDNSDomainsCritical = []string{"idp.bank.example"}, true
	}})
	keyDER, err := x509.MarshalPKCS8PrivateKey(r.inter.Key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(r.pki, "certs/root.crt"), "CERTIFICATE", r.root.DER, 0o644)
	writePEM(t, filepath.Join(r.pki, "certs/intermediate.crt"), "CERTIFICATE", r.inter.DER, 0o644)
	writePEM(t, filepath.Join(r.pki, "secrets/intermediate.key"), "PRIVATE KEY", keyDER, 0o644) // a test key; a bank keeps this in an HSM or KMS

	tplSrc, err := os.ReadFile(filepath.Join(here, "ca/templates/approver-leaf.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.pki, "templates/approver-leaf.tpl"), tplSrc, 0o644); err != nil {
		t.Fatal(err)
	}
	opPub, opEnc := r.operatorKey()
	tpl2, err := os.ReadFile(filepath.Join(here, "ca/templates/operator-leaf.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.pki, "templates/operator-leaf.tpl"), tpl2, 0o644); err != nil {
		t.Fatal(err)
	}
	render(t, filepath.Join(here, "ca/ca.json.tmpl"), filepath.Join(r.pki, "config/ca.json"),
		map[string]any{"CRLCache": "10m", "CRLRenew": "5m", "Provisioners": provisioners, "OperatorPublic": opPub, "OperatorEncrypted": opEnc})
	render(t, filepath.Join(here, "idp/config.json.tmpl"), filepath.Join(r.dir, "idp.json"), map[string]any{"Clients": clients})

	id := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	r.idpName, r.caName = "ssocerts-idp-"+id, "ssocerts-ca-"+id
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", r.idpName, r.caName).Run() })

	cfg, err := os.ReadFile(filepath.Join(r.dir, "idp.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The IdP's namespace is shared by the CA and the step CLI, so "localhost" is the IdP for all three
	// (the step CLI accepts only an https provider, and the mock's certificate is for localhost). Port 19001
	// is published because the CRL's distribution point, in every leaf, is http://localhost:19001/1.0/crl.
	run(t, "docker", "run", "-d", "--name", r.idpName, "-e", "JSON_CONFIG="+string(cfg),
		"-p", "127.0.0.1::8080", "-p", "127.0.0.1:19001:19001", imgIdP)
	idpAddr := strings.TrimSpace(run(t, "docker", "port", r.idpName, "8080/tcp"))
	idpAddr = strings.Split(idpAddr, "\n")[0]
	r.idpPEM = filepath.Join(r.pki, "certs/idp.pem")
	r.fetchIdPCert(idpAddr)

	run(t, "docker", "run", "-d", "--name", r.caName, "--network", "container:"+r.idpName,
		"-v", r.pki+":/pki", "-e", "SSL_CERT_FILE=/pki/certs/idp.pem", "--entrypoint", "step-ca", imgCA, "/pki/config/ca.json")
	r.waitFor("step-ca", func() error {
		resp, err := http.Get(crlURL)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})

	// The step CLI runs in a container that shares the IdP's network namespace, with a stand-in for the
	// browser: step opens the authorization URL with xdg-open, and the stand-in follows it with curl, which
	// ends at the CLI's own loopback listener.
	xdg := filepath.Join(r.dir, "xdg-open")
	if err := os.WriteFile(xdg, []byte("#!/bin/sh\ncurl -sS -L --cacert \"$IDP_CA\" -o /dev/null \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.step = filepath.Join(r.dir, "step")
	wrapper := fmt.Sprintf(`#!/bin/sh
exec docker run --rm ${STEP_NOTTY:--t} --user %d:%d --network container:%s -v %q:%q -v %q:/usr/local/bin/xdg-open:ro -v %q:/idp.pem:ro \
  -e IDP_CA=/idp.pem -e SSL_CERT_FILE=/idp.pem -e HOME=%q -e STEPPATH=%q -w "$PWD" %s step "$@"
`, os.Getuid(), os.Getgid(), r.idpName, r.dir, r.dir, xdg, r.idpPEM, r.dir, filepath.Join(r.dir, ".step"), imgCLI)
	if err := os.WriteFile(r.step, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	r.bootstrap()
	return r
}

func (r *rig) fetchIdPCert(addr string) {
	r.t.Helper()
	r.waitFor("the IdP", func() error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"})
		if err != nil {
			return err
		}
		defer conn.Close()
		certs := conn.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			return errors.New("no certificate")
		}
		writePEM(r.t, r.idpPEM, "CERTIFICATE", certs[0].Raw, 0o644)
		return nil
	})
}

func (r *rig) waitFor(what string, f func() error) {
	r.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "40", r.caName).CombinedOutput()
	r.t.Fatalf("%s did not come up: %v\nCA log:\n%s", what, err, logs)
}

// bootstrap points the step CLI at the CA by the root's fingerprint, as an approver's workstation is set up once.
func (r *rig) bootstrap() {
	r.t.Helper()
	fp := fingerprint(r.root.Cert)
	run(r.t, r.step, "ca", "bootstrap", "--ca-url", "https://localhost:19000", "--fingerprint", fp, "--force")
}

func fingerprint(c *x509.Certificate) string {
	return fmt.Sprintf("%x", sha256Sum(c.Raw))
}

// leafOf parses the first certificate of a PEM file.
func leafOf(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s: no certificate", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// renew runs scripts/renew.sh for the approver ops through provisioner prov into out, with the pre-flight
// check skipped (the test runs bide identity check itself), and returns its output and error.
func (r *rig) renew(prov, out string) (string, error) {
	r.t.Helper()
	here, _ := os.Getwd()
	env := []string{
		"APPROVER=ops", "POLICY=" + r.policy, "PROVISIONER=" + prov, "OUT=" + out,
		"ISSUING_CA=" + filepath.Join(r.pki, "certs/intermediate.crt"),
		"STEP=" + r.step, "BIDE=" + r.bide, "BIDE_HSM=" + r.bideHSM, "SKIP_CHECK=1",
	}
	return tryRun(env, r.dir, "sh", filepath.Join(here, "scripts/renew.sh"))
}

// writeGateFiles writes the certificate policy file and the CA timings file the gate and bide identity
// check take, for this PKI. The CRL sources are the CA's own CRL (the URL its leaves and its CRL name) and
// the root's, a static file the bank's PKI team publishes; rootCRLURL is where.
func (r *rig) writeGateFiles(rootCRLURL string) {
	r.t.Helper()
	rootK, interK := spkiHex(r.root.Cert), spkiHex(r.inter.Cert)
	writePEM(r.t, filepath.Join(r.dir, "root.pem"), "CERTIFICATE", r.root.DER, 0o644)
	writePEM(r.t, filepath.Join(r.dir, "inter.pem"), "CERTIFICATE", r.inter.DER, 0o644)
	policy := map[string]any{
		"anchors": []string{"root.pem"}, "intermediates": []string{interK}, "intermediate_certs": []string{"inter.pem"},
		"identity": map[string]string{"ops": testca.Person(1)}, "id_kind": "person-uri",
		"eku": testca.EKU.String(), "policy_oid": testca.PolicyOID.String(),
		"canonical":                map[string]string{"host": "idp.bank.example"},
		"max_leaf_lifetime":        "8h",
		"require_name_constraints": true, "require_policy_on_cas": true,
		"crl": map[string]any{
			rootK:  map[string]any{"urls": []string{rootCRLURL}},
			interK: map[string]any{"urls": []string{crlURL}},
		},
	}
	params := map[string]any{
		"ca_period": "10m", "max_status_age": "30m", "exec_status_age": "30m",
		"issuers": map[string]any{
			rootK:  map[string]any{"ca_period": "24h", "max_status_age": "96h", "exec_status_age": "96h", "ca_overlap": "0s"},
			interK: map[string]any{"ca_period": "10m", "max_status_age": "30m", "exec_status_age": "30m", "ca_overlap": "0s"},
		},
	}
	r.policy, r.params = filepath.Join(r.dir, "policy.json"), filepath.Join(r.dir, "ca-timings.json")
	for path, v := range map[string]any{r.policy: policy, r.params: params} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
}

func spkiHex(c *x509.Certificate) string {
	h := audit.SPKIHashOf(c)
	return hex.EncodeToString(h[:])
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// identityCheck runs bide identity check -json over the chain (no CRLs, no recorded gate policy: a
// partial result, exit 4) and returns the checks by id.
func (r *rig) identityCheck(chain string, extra ...string) (map[string]checkResult, int) {
	r.t.Helper()
	return r.identityCheckWith(chain, append([]string{"-no-status"}, extra...)...)
}

// identityCheckWith is identityCheck with the choice of where the CRLs come from (-crl-dir, -fetch or -no-status).
func (r *rig) identityCheckWith(chain string, extra ...string) (map[string]checkResult, int) {
	r.t.Helper()
	args := append([]string{"identity", "check", "-policy", r.policy, "-params", r.params, "-no-expect", "-cert", chain, "-as", "ops", "-json"}, extra...)
	cmd := exec.Command(r.bide, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("bide identity check: %v", err)
	}
	var rep struct {
		Checks []checkResult `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		r.t.Fatalf("bide identity check -json: %v\n%s\n%s", err, out.String(), errb.String())
	}
	m := map[string]checkResult{}
	for _, c := range rep.Checks {
		m[c.ID] = c
	}
	return m, code
}

type checkResult struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// TestIssuance is the recipe's first half: an approver gets a certificate from the OIDC login that is
// exactly what the gate's policy accepts, and a login that falls short gets none.
func TestIssuance(t *testing.T) {
	r := newRig(t)
	r.writeGateFiles("http://localhost:1/root.crl")

	for _, alg := range []string{"ecdsa-p256", "ed25519"} {
		t.Run("approver "+alg, func(t *testing.T) {
			out := filepath.Join(r.dir, "approver-"+alg)
			t.Setenv("KEY_ALG", alg)
			log, err := r.renew("sso", out)
			if err != nil {
				t.Fatalf("renew.sh: %v\n%s", err, log)
			}
			chain := filepath.Join(out, "ops-chain.pem")
			leaf := leafOf(t, chain)
			checkLeaf(t, leaf, r.inter.Cert)
			if want := map[string]x509.PublicKeyAlgorithm{"ecdsa-p256": x509.ECDSA, "ed25519": x509.Ed25519}[alg]; leaf.PublicKeyAlgorithm != want {
				t.Fatalf("the certificate's key is %v, want %v: the request's key was not used", leaf.PublicKeyAlgorithm, want)
			}

			// The gate's own chain rule accepts it for ops (the check is partial: no CRLs, no recorded policy).
			checks, code := r.identityCheck(chain)
			if c := checks["cert.chain"]; c.Status != "OK" {
				t.Fatalf("the gate would not accept the certificate: %+v", c)
			}
			if code != 4 {
				t.Fatalf("bide identity check exit %d, want 4 (partial: no CRLs, no recorded policy)", code)
			}
		})
	}

	for name, tc := range map[string]struct{ prov, why string }{
		"no MFA":                {"sso-nomfa", "did not use MFA"},
		"a malformed person id": {"sso-badid", "person_id claim is not 32 lowercase hex digits"},
	} {
		prov := tc.prov
		t.Run("refused: "+name, func(t *testing.T) {
			out := filepath.Join(r.dir, "refused-"+prov)
			log, err := r.renew(prov, out)
			if err == nil {
				t.Fatalf("a certificate was issued:\n%s", log)
			}
			t.Logf("%s", log)
			if !strings.Contains(log, tc.why) {
				t.Fatalf("refused, but not by the template (want %q):\n%s", tc.why, log)
			}
			if ents, _ := filepath.Glob(filepath.Join(out, "*.crt")); len(ents) != 0 {
				t.Fatalf("a refused login left a certificate: %v", ents)
			}
		})
	}
}

// checkLeaf asserts the shape of an approver certificate against what an IdentityPolicy requires.
func checkLeaf(t *testing.T, c, issuer *x509.Certificate) {
	t.Helper()
	if len(c.URIs) != 1 || c.URIs[0].String() != testca.Person(1) || len(c.DNSNames)+len(c.EmailAddresses)+len(c.IPAddresses) != 0 {
		t.Errorf("SANs: uris %v dns %v email %v ip %v, want exactly %s", c.URIs, c.DNSNames, c.EmailAddresses, c.IPAddresses, testca.Person(1))
	}
	if c.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("key usage %v, want digitalSignature only", c.KeyUsage)
	}
	if len(c.ExtKeyUsage) != 0 || len(c.UnknownExtKeyUsage) != 1 || !c.UnknownExtKeyUsage[0].Equal(testca.EKU) {
		t.Errorf("EKU %v and %v, want only %v", c.ExtKeyUsage, c.UnknownExtKeyUsage, testca.EKU)
	}
	if len(c.Policies) != 1 || !c.Policies[0].Equal(testca.PolicyOID) {
		t.Errorf("policies %v, want %v", c.Policies, testca.PolicyOID)
	}
	if len(c.CRLDistributionPoints) != 1 || c.CRLDistributionPoints[0] != crlURL {
		t.Errorf("CRL distribution points %v, want [%s]", c.CRLDistributionPoints, crlURL)
	}
	if c.Subject.CommonName != "" || len(c.Subject.Names) != 0 {
		t.Errorf("subject %v, want empty", c.Subject)
	}
	// step-ca backdates notBefore by 1m and counts the duration from issue: span = duration + 1m. The CA
	// is set to 7h59m so the span is within the policy's 8h.
	if span := c.NotAfter.Sub(c.NotBefore); span > 8*time.Hour || span < 7*time.Hour {
		t.Errorf("lifetime %v, want about 8h and at most the policy's 8h", span)
	}
	if err := c.CheckSignatureFrom(issuer); err != nil {
		t.Errorf("not issued by the issuing CA: %v", err)
	}
	if c.IsCA {
		t.Error("the leaf is a CA")
	}
	var _ asn1.ObjectIdentifier
}

// operatorKey makes the operator's JWK: its public JWK (JSON) and its private key as a compact JWE, which
// the CA configuration holds, protected by the password in r.dir/operator.pw. A test key; a bank keeps the
// provisioner's key where only its CA operators reach it.
func (r *rig) operatorKey() (pub, enc string) {
	r.t.Helper()
	pw := filepath.Join(r.dir, "operator.pw")
	if err := os.WriteFile(pw, []byte("operator-password-for-the-test-ca"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	run(r.t, "docker", "run", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-v", r.dir+":"+r.dir, "-w", r.dir, imgCLI,
		"step", "crypto", "jwk", "create", "operator.pub.json", "operator.key.json", "--password-file", pw, "--use", "sig", "--kty", "EC", "--crv", "P-256", "--force")
	pubB, err := os.ReadFile(filepath.Join(r.dir, "operator.pub.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	var k map[string]string
	b, err := os.ReadFile(filepath.Join(r.dir, "operator.key.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := json.Unmarshal(b, &k); err != nil {
		r.t.Fatal(err)
	}
	enc = strings.Join([]string{k["protected"], k["encrypted_key"], k["iv"], k["ciphertext"], k["tag"]}, ".")
	return strings.TrimSpace(string(pubB)), enc
}

// revoke revokes the certificate with the given serial (decimal) as a CA operator: the JWK provisioner's
// token, made with its password file, then the revocation. The token is on this command's line, which is
// acceptable for a throwaway CA only; an operator runs `step ca revoke <serial>`, which asks for the
// provisioner and its password on the terminal.
func (r *rig) revoke(serial, reason string) {
	r.t.Helper()
	pw := filepath.Join(r.dir, "operator.pw")
	script := fmt.Sprintf(`set -e; cd %q; T=$(%q ca token --provisioner operator --provisioner-password-file %q --revoke %s | tail -n 1 | tr -d '\r'); %q ca revoke --token "$T" --reasonCode=%s --reason=test %s`,
		r.dir, r.step, pw, serial, r.step, reason, serial)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "STEP_NOTTY=-i")
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("revoke: %v\n%s", err, out)
	}
}
