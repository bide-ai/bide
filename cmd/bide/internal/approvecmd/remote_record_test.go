package approvecmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
