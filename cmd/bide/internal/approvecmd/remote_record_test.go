package approvecmd

import (
	"crypto/elliptic"
	"encoding/json"
	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/approvals"
)

// R2: an endpoint that names any record, consistently in Response.Record and in the receipt, gets
// the receipt written. Before, Record had to be agent.DecisionStep(d); now only non-empty and
// self-consistent, so a name that is not a record of this decision in any journal is accepted.
func TestArbitraryConsistentRecordNameIsNotWritten(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	srv := f.intake(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			if r.Method != http.MethodPost {
				w.Write(rec.Body.Bytes())
				return
			}
			var resp approvals.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Error(err)
			}
			resp.Record, resp.Receipt.Record = "approval:bogus\x1b[31m", "approval:bogus\x1b[31m"
			writeTestJSON(w, resp)
		})
	})
	code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...)
	m, _ := filepath.Glob(filepath.Join(f.dir, "*.receipt.json"))
	if code == 0 || len(m) != 0 {
		t.Fatalf("exit %d, receipts %v: a receipt naming a record that is not the decision's was written\n%s", code, m, errs)
	}
}

// A hostile endpoint returns the high-S twin (r, n-s) of the approver's ECDSA signature in the
// receipt: it verifies as a plain ECDSA signature but is not the canonical form, so the scheme's
// verifier refuses it and no receipt is written.
func TestRemote_HighSTwinInTheReceiptIsRefused(t *testing.T) {
	f := newFixtureAlg(t, twoOfThree, refundArgs, audit.AlgECDSAP256)
	srv := f.intake(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			if r.Method != http.MethodPost {
				w.Write(rec.Body.Bytes())
				return
			}
			var resp approvals.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Error(err)
			}
			n := elliptic.P256().Params().N
			sv := new(big.Int).SetBytes(resp.Receipt.Signature[32:])
			twin := append([]byte(nil), resp.Receipt.Signature...)
			new(big.Int).Sub(n, sv).FillBytes(twin[32:])
			resp.Receipt.Signature = twin
			writeTestJSON(w, resp)
		})
	})
	code, _, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...)
	m, _ := filepath.Glob(filepath.Join(f.dir, "*.receipt.json"))
	if code != 1 || len(m) != 0 || !strings.Contains(errs, "does not verify") {
		t.Fatalf("exit %d, receipts %v\n%s", code, m, errs)
	}
}

// A retry of a randomized scheme's decision is answered from the record journaled first, whose
// signature differs from the one just sent: the receipt holds the recorded one, and the command says
// so on one line.
func TestRemote_RecordedSignatureDifferingFromTheSentOneIsNoted(t *testing.T) {
	f := newFixtureAlg(t, twoOfThree, refundArgs, audit.AlgECDSAP256)
	sg, err := LoadSigner(f.keys["ops"])
	if err != nil {
		t.Fatal(err)
	}
	srv := f.intake(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			if r.Method != http.MethodPost {
				w.Write(rec.Body.Bytes())
				return
			}
			var resp approvals.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Error(err)
			}
			rc := resp.Receipt
			at, _ := agent.ParseTimestamp(rc.DecidedAt)
			s := agent.ApprovalSubject{RunID: rc.RunID, ToolUseID: rc.ToolUseID, ToolName: rc.ToolName, Args: rc.Args}
			rc.Signature, _ = sg.Sign(agent.ApprovalDecisionBytes(s, rc.Approver, rc.Approved, at)) // the same decision, other bytes
			writeTestJSON(w, resp)
		})
	})
	code, out, errs := f.runTo("", f.to(srv.URL, "ops", "-yes")...)
	if code != 0 || strings.Count(out, "differs from the one just sent") != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}
