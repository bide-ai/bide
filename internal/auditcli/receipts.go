package auditcli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// receiptSuffix is how bide approve names the receipts it writes; a directory given to -receipt is
// searched for files that end in it.
const receiptSuffix = ".receipt.json"

// verifyReceipts checks decision receipts (audit.DecisionReceipt, written by bide approve) against
// a journal export and the signed tree heads of the run: each receipt's signature verifies for the
// approver's key, and the heads commit to the decision where the receipt says it was journaled
// (audit.CheckReceipt).
func (c *cli) verifyReceipts(args []string) {
	fs := c.flagSet("verify-receipts")
	var receipts, journals, heads stringList
	fs.Var(&receipts, "receipt", "a decision receipt file, or a directory of *"+receiptSuffix+" files; repeatable")
	fs.Var(&journals, "journal", "the run's journal export (audit.ExportJournal), one per run the receipts name; repeatable")
	fs.Var(&heads, "heads", "signed tree heads of the journal: a file with one head, or one head per line (the anchor log a published head is appended to); repeatable")
	pubkey := fs.String("pubkey", "", "the log's "+pubkeyHelp)
	keysPath := fs.String("approver-keys", "", `JSON object of approver id to public key text (<scheme>:<hex>), e.g. {"ops":"ed25519:ab12..."}`)
	skew := fs.Duration("skew", audit.DefaultReceiptSkew, "how far a head's time may fall short of a receipt's recorded_at before a head that omits the decision counts as signed after it")
	c.parse(fs, args)

	c.require(fs, required{"-receipt", len(receipts) == 0}, required{"-journal", len(journals) == 0}, required{"-heads", len(heads) == 0}, absent("-pubkey", *pubkey), absent("-approver-keys", *keysPath))
	if *skew <= 0 {
		c.usageError("verify-receipts: -skew must be positive")
	}

	logKey, err := readPubKey(*pubkey)
	ok := c.note(err)
	var keyText map[string]string
	ok = c.note(c.readJSON(*keysPath, &keyText)) && ok
	keys := map[string]audit.Verifier{}
	for id, text := range keyText {
		k, err := audit.ParsePublicKey(text)
		if err != nil {
			ok = c.note(unusable(fmt.Errorf("approver %q key: %w", id, err))) && ok
			continue
		}
		keys[id] = k
	}
	files, err := receiptFiles(receipts)
	ok = c.note(err) && ok
	byRun := map[string][]agent.Record{}
	for _, p := range journals {
		var x audit.JournalExport
		if err := c.readJSON(p, &x); err != nil {
			ok = c.note(err) && ok
			continue
		}
		recs, err := x.Journal()
		if err != nil {
			ok = c.note(unusable(fmt.Errorf("%s: %w", p, err))) && ok
			continue
		}
		byRun[x.RunID] = recs
	}
	var signed []audit.SignedTreeHead
	for _, p := range heads {
		hs, err := c.readHeads(p)
		ok = c.note(err) && ok
		signed = append(signed, hs...)
	}
	var rs []audit.DecisionReceipt
	for _, p := range files {
		var r audit.DecisionReceipt
		if err := c.readJSON(p, &r); err != nil {
			ok = c.note(err) && ok
			continue
		}
		rs = append(rs, r)
	}
	if !ok || !c.clean() {
		return
	}

	failed, pending := 0, 0
	for _, r := range rs {
		what := fmt.Sprintf("%s %s on call %s of run %s (record %s)", cleanText(r.Approver), approvedWord(r.Approved), cleanText(r.ToolUseID), cleanText(r.RunID), cleanText(r.Record))
		v, known := keys[r.Approver]
		if !known {
			c.printf("FAIL  %s: -approver-keys has no key for this approver\n", what)
			failed++
			continue
		}
		if err := r.Verify(v); err != nil {
			c.printf("FAIL  %s: %s\n", what, cleanText(err.Error()))
			failed++
			continue
		}
		journal, have := byRun[r.RunID]
		if !have {
			c.note(unusable(fmt.Errorf("no -journal for run %q, which receipt %s names", r.RunID, r.Record)))
			continue
		}
		f := audit.CheckReceipt(r, journal, signed, logKey, *skew)
		switch {
		case f.Verdict == audit.ReceiptAnchored:
			c.printf("PASS  %s: signature verifies; %s\n", what, cleanText(f.Detail))
		case f.Verdict == audit.ReceiptPending:
			c.printf("INFO  %s: signature verifies; pending: %s\n", what, cleanText(f.Detail))
			pending++
		default:
			c.printf("FAIL  %s: %s: %s\n", what, f.Verdict, cleanText(f.Detail))
			failed++
		}
	}
	if !c.clean() {
		return
	}
	if failed > 0 {
		c.fail("%d of %d receipt(s) did not verify or are not committed to by the signed heads", failed, len(rs))
		return
	}
	if pending > 0 {
		c.printf("PASS: %d receipt(s) verified; %d not yet signed into a head (the application's next write to the run does it)\n", len(rs), pending)
		return
	}
	c.printf("PASS: %d receipt(s) verified and committed to by signed heads\n", len(rs))
}

func approvedWord(approved bool) string {
	if approved {
		return "approved"
	}
	return "denied"
}

// receiptFiles expands -receipt: a file is itself, a directory is its *.receipt.json files.
func receiptFiles(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, unusable(fmt.Errorf("-receipt %s: %w", p, err))
		}
		if !fi.IsDir() {
			out = append(out, p)
			continue
		}
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, unusable(fmt.Errorf("-receipt %s: %w", p, err))
		}
		var in []string
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), receiptSuffix) {
				in = append(in, filepath.Join(p, e.Name()))
			}
		}
		if len(in) == 0 {
			return nil, unusable(fmt.Errorf("-receipt %s holds no *%s file", p, receiptSuffix))
		}
		sort.Strings(in)
		out = append(out, in...)
	}
	return out, nil
}

// readHeads reads signed tree heads from a file: one head, or one head per line.
func (c *cli) readHeads(path string) ([]audit.SignedTreeHead, error) {
	b, err := readInput(path)
	if err != nil {
		return nil, err
	}
	var one audit.SignedTreeHead
	if err := audit.UnmarshalStrict(b, &one); err == nil {
		if err := checkHeadTimes(reflect.ValueOf(&one), time.Now()); err != nil {
			return nil, &cliError{class: errNotVerified, err: fmt.Errorf("%s: %w", path, err)}
		}
		return []audit.SignedTreeHead{one}, nil
	}
	var out []audit.SignedTreeHead
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var h audit.SignedTreeHead
		if err := audit.UnmarshalStrict(line, &h); err != nil {
			return nil, unusable(fmt.Errorf("parse %s line %d: %w; want a signed tree head, or one per line", path, i+1, err))
		}
		if err := checkHeadTimes(reflect.ValueOf(&h), time.Now()); err != nil {
			return nil, &cliError{class: errNotVerified, err: fmt.Errorf("%s line %d: %w", path, i+1, err)}
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, unusable(fmt.Errorf("%s holds no signed tree head", path))
	}
	return out, nil
}
