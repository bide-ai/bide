package auditcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
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
	policyPath := fs.String("identity-policy", "", "the certificate policy file (trust anchors, pinned intermediates, the approver-to-identity map; see docs/guides/hitl-approval.md): required for a certificate-bound receipt, which is checked against it and the log key instead of -approver-keys. The check is the identity at the time the decision became provable; revocation status is not checked")
	update := fs.Bool("update", false, "write an anchoring proof into each receipt whose decision a signed head now commits to (an atomic rewrite of the receipt file, mode kept): from then on a signed head that omits the decision proves its deletion; without it the verdict is the approver's claim")
	c.parse(fs, args)

	c.require(fs, required{"-receipt", len(receipts) == 0}, required{"-journal", len(journals) == 0}, required{"-heads", len(heads) == 0}, absent("-pubkey", *pubkey), required{"-approver-keys or -identity-policy", *keysPath == "" && *policyPath == ""})

	logKey, err := readPubKey(*pubkey)
	ok := c.note(err)
	var keyText map[string]string
	if *keysPath != "" {
		ok = c.note(c.readJSON(*keysPath, &keyText)) && ok
	}
	var ci *audit.CertIdentity
	if *policyPath != "" {
		var perr error
		if ci, perr = policyfile.Read(*policyPath); perr != nil {
			ok = c.note(unusable(perr)) && ok
		}
	}
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
	var paths []string
	for _, p := range files {
		var r audit.DecisionReceipt
		if err := c.readJSON(p, &r); err != nil {
			ok = c.note(err) && ok
			continue
		}
		rs = append(rs, r)
		paths = append(paths, p)
	}
	if !ok || !c.clean() {
		return
	}

	c.reportUnheadedTails(byRun, signed, logKey)
	failed, pending, accepted := 0, 0, 0
	for i, r := range rs {
		what := fmt.Sprintf("%s %s on call %s of run %s (record %s)", cleanText(r.Approver), approvedWord(r.Approved), cleanText(r.ToolUseID), cleanText(r.RunID), cleanText(r.Record))
		if len(r.Cert) > 0 {
			switch outcome := c.certReceipt(what, r, ci, byRun[r.RunID], signed, logKey); outcome {
			case certFailed:
				failed++
			case certPending:
				pending++
			case certAccepted:
				accepted++
			}
			continue
		}
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
		f := audit.CheckReceipt(r, journal, signed, logKey)
		switch {
		case f.Verdict == audit.ReceiptAnchored || f.Verdict == audit.ReceiptRedacted:
			c.printf("PASS  %s: signature verifies; %s: %s\n", what, f.Verdict, cleanText(f.Detail))
			if *update && r.Anchor == nil && f.Verdict == audit.ReceiptAnchored {
				if err := anchorReceiptFile(paths[i], r, journal, signed, logKey); err != nil {
					c.printf("FAIL  %s: the anchoring proof was not stored: %s\n", what, cleanText(err.Error()))
					failed++
				} else {
					c.printf("INFO  %s: anchoring proof stored in %s\n", what, cleanText(paths[i]))
				}
			}
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
	// A certificate-bound receipt is counted apart: its identity was accepted at A, with revocation not checked.
	verified := len(rs) - accepted - pending
	note := ""
	if accepted > 0 {
		note = fmt.Sprintf("; %d certificate-bound receipt(s) had their identity accepted at the time the decision became provable, revocation status not checked (not verified decisions)", accepted)
	}
	if pending > 0 {
		c.printf("PASS: %d receipt(s) verified%s; %d not yet signed into a head (the application's next write to the run, or a recovery pass, does it)\n", verified, note, pending)
		return
	}
	c.printf("PASS: %d receipt(s) verified and committed to by signed heads%s\n", verified, note)
}

// anchorReceiptFile adds the anchoring proof to the receipt file at path, by writing a temporary file
// beside it with the receipt's mode and renaming it over the receipt, so a crash leaves the old
// receipt or the new one. It adds one field; nothing the approver signed or claimed changes.
func anchorReceiptFile(path string, r audit.DecisionReceipt, journal []agent.Record, heads []audit.SignedTreeHead, logKey audit.Verifier) error {
	out, err := audit.AnchorReceipt(r, journal, heads, logKey)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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

// Outcomes of checking one certificate-bound receipt.
const (
	certOK = iota
	certAccepted
	certFailed
	certPending
)

// certReceipt checks a certificate-bound receipt: the journal and heads must commit to the decision (as
// for a bare-key receipt), and the chain it carries must be accepted by the policy at A, the time of the
// head that covers it (audit.DecisionReceipt.VerifyIdentity). Revocation status is not looked at, and
// the line says so: this is neither a final nor a provisional verdict.
func (c *cli) certReceipt(what string, r audit.DecisionReceipt, ci *audit.CertIdentity, journal []agent.Record, heads []audit.SignedTreeHead, logKey audit.Verifier) int {
	if ci == nil {
		c.printf("FAIL  %s: the receipt is certificate-bound; pass -identity-policy (the certificate policy file) to check its chain and identity\n", what)
		return certFailed
	}
	if journal == nil {
		c.note(unusable(fmt.Errorf("no -journal for run %q, which receipt %s names", r.RunID, r.Record)))
		return certOK
	}
	f := audit.CheckReceipt(r, journal, heads, logKey)
	switch f.Verdict {
	case audit.ReceiptPending:
		c.printf("INFO  %s: pending: %s (the identity is checked once a signed head covers the decision)\n", what, cleanText(f.Detail))
		return certPending
	case audit.ReceiptAnchored, audit.ReceiptRedacted:
	default:
		c.printf("FAIL  %s: %s: %s\n", what, f.Verdict, cleanText(f.Detail))
		return certFailed
	}
	// A comes from the heads the caller supplied too, and the earlier of that and the receipt's own
	// anchoring proof is used: the auditor's own list is the better source, and a later head only fails closed.
	own := r.Anchor
	bare := r
	bare.Anchor = nil
	fromHeads, err := audit.AnchorReceipt(bare, journal, heads, logKey)
	switch {
	case err == nil && own == nil:
		r = fromHeads
	case err == nil:
		if a1, e1 := r.AnchorTime(logKey); e1 != nil || fromHeads.Anchor.STH.TimestampNanos < a1.UnixNano() {
			r = fromHeads
		}
	case own == nil:
		c.printf("FAIL  %s: %s\n", what, cleanText(err.Error()))
		return certFailed
	}
	id, err := r.VerifyIdentity(ci, logKey)
	if err != nil {
		c.printf("FAIL  %s: %s\n", what, cleanText(err.Error()))
		return certFailed
	}
	name := ""
	if id.DisplayName != "" {
		name = fmt.Sprintf(" (certificate name %s, not part of the identity)", strconv.Quote(cleanText(id.DisplayName)))
	}
	c.printf("IDENTITY  %s: %s; identity %s%s accepted at %s (%s), certificate valid %s to %s, leaf %s; revocation status NOT checked, so this is not the gate's verdict\n", what, f.Verdict,
		cleanText(id.Identifier), name, agent.FormatTimestamp(id.AnchoredAt), id.TimeSource, agent.FormatTimestamp(id.NotBefore), agent.FormatTimestamp(id.NotAfter), id.LeafHash[:16])
	return certAccepted
}

// reportUnheadedTails prints, for each journal given, how many of its records are after the newest
// signed head that verifies under logKey: the tail that coalescing (a minimum interval between heads),
// an anchor outage or a process that stopped leaves under no head. A receipt for a decision in that tail
// is pending, not failed; this line says how far the tail goes. It reads only the heads of the journal's
// own run and fails nothing.
func (c *cli) reportUnheadedTails(byRun map[string][]agent.Record, heads []audit.SignedTreeHead, logKey audit.Verifier) {
	runs := make([]string, 0, len(byRun))
	for r := range byRun {
		runs = append(runs, r)
	}
	slices.Sort(runs)
	for _, r := range runs {
		// A head counts only if it verifies, is this run's, and is the root of this journal's own prefix
		// (a mismatched head covers nothing here): the rule verify-decision-coverage uses.
		rep, err := audit.CheckDecisionCoverage(r, byRun[r], heads, logKey, audit.CoverageBound{})
		if err != nil {
			continue
		}
		newest := rep.LargestHead
		if tail := len(byRun[r]) - newest; tail > 0 {
			var signedAt time.Time
			for _, h := range heads {
				if h.RunID == r && h.Kind == audit.TreeJournal && h.Size == newest && h.Verify(logKey) == nil {
					if t := time.Unix(0, h.TimestampNanos); t.After(signedAt) {
						signedAt = t
					}
				}
			}
			when := "no counted head covers any of it"
			if newest > 0 && !signedAt.IsZero() {
				when = fmt.Sprintf("the newest counted head covers %d and was signed %s ago", newest, time.Since(signedAt).Round(time.Second))
			}
			c.printf("INFO  run %s: records %d to %d (%d, positions from 0) are under no signed head yet; %s (a head held back by a minimum interval, or a failed publish: verify-decision-coverage -max-unanchored tells them apart)\n", cleanText(r), newest, len(byRun[r])-1, tail, when)
		}
	}
}
