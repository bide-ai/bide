// Package identitypreflight is the engine of bide identity check: a pre-flight for a certificate-bound
// approval setup, run before go-live and when an approver's decision is refused. It reports, one check at a
// time, whether the identity policy file loads, the pinned CAs and intermediates match, the CRLs are
// reachable and fresh enough for the declared status ages, the declared values agree with the gate's
// recorded policy, and a given approver certificate would be accepted.
//
// It decides nothing itself. Every verdict is the gate's own: the policy is compiled by
// audit.ParseIdentityPolicy, the declared timings are validated by audit.CertApprover.ValidateConfig, CRLs
// are fetched and judged by the crlrefresh package, and a certificate chain is judged by
// audit.CertApprover.CheckCertificate, the function the gate's counting and execution checks call. This
// package reads files and fetches CRLs (it is where that I/O lives, not audit), orders the answers, and
// words the fix for each.
package identitypreflight

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bide-ai/bide/internal/term"
)

// Status is the outcome of one check.
type Status string

// The outcomes. Fail means a gate built from this setup would refuse, pause or miscount; Warn means it
// works but will not for long or differs from a recorded value in a way that does not reach a verdict;
// Skip means the check did not run, and Report.Partial says whether that makes the report incomplete.
const (
	OK   Status = "OK"
	Warn Status = "WARN"
	Fail Status = "FAIL"
	Skip Status = "SKIP"
)

// Check is one line of the report. ID is stable (the troubleshooting table of the certificate approvers
// guide is keyed by it); Fix is empty for an OK.
type Check struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Report is the result of Run.
type Report struct {
	// Ready is true only when no check failed and nothing the gate relies on was left unchecked.
	Ready bool `json:"ready"`
	// Partial is true when the caller acknowledged leaving a part out (-no-status, -no-expect): the report
	// is then never ready, whatever else passed.
	Partial   bool      `json:"partial"`
	CheckedAt time.Time `json:"checked_at"`
	Checks    []Check   `json:"checks"`
	// NotCheckable lists what no pre-flight can establish; it is part of every report.
	NotCheckable []string `json:"not_checkable"`
}

// NotCheckable is what a pre-flight cannot establish ahead of time. It is printed with every report.
var NotCheckable = []string{
	"the signature of a real decision, and that the approver's key is the certificate's key (no decision exists yet)",
	"when a real decision becomes provable: the signed head that will cover it, the log clock, and an independent time source (none is shipped)",
	"whether a future decision will be provisional or final: that depends on revocations published after it and on the window W",
	"revocations published after this check, and whether the CRLs stay reachable from the gate host (run -fetch on the gate host; the application's own HTTP client, proxy and DNS rules are not exercised)",
	"the gate's other settings unless -expect supplies them: separation of duties, the roster, requester mapping, holds, and the GODEBUG of the gate process",
	"that the gate embeds the CRL refresher or that a fetch-crl job is running",
}

// Failed reports whether any check failed.
func (r *Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Exit codes of bide identity check.
const (
	ExitReady   = 0 // no check failed and nothing was left unchecked
	ExitFail    = 1 // at least one check failed: the setup will not work as configured
	ExitUsage   = 2 // a usage error
	ExitIO      = 3 // a file could not be read, or the tool could not run
	ExitPartial = 4 // no check failed, but a part was acknowledged as not checked (-no-status, -no-expect)
)

// ExitCode is the exit status for a report.
func (r *Report) ExitCode() int {
	switch {
	case r.Failed():
		return ExitFail
	case r.Partial:
		return ExitPartial
	}
	return ExitReady
}

func (r *Report) add(id string, s Status, fix string, format string, a ...any) {
	r.Checks = append(r.Checks, Check{ID: id, Status: s, Message: fmt.Sprintf(format, a...), Fix: fix})
}

func (r *Report) finish() {
	r.Ready = !r.Failed() && !r.Partial
	r.NotCheckable = NotCheckable
}

// JSON writes the report as one indented JSON document.
func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// Text writes the report for a person: one line per check, the fix under a WARN or FAIL, the summary, and
// what cannot be checked ahead of time.
func (r *Report) Text(w io.Writer, st term.Style) {
	width := 0
	for _, c := range r.Checks {
		width = max(width, len(c.ID))
	}
	paint := map[Status]func(string) string{OK: st.OK, Warn: st.Warn, Fail: st.Fail, Skip: st.Dim}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%s  %-*s  %s\n", paint[c.Status](fmt.Sprintf("%-4s", string(c.Status))), width, c.ID, c.Message)
		if c.Fix != "" {
			fmt.Fprintf(w, "      %s\n", st.Dim("fix: "+c.Fix))
		}
	}
	fmt.Fprintln(w)
	switch {
	case r.Failed():
		fmt.Fprintln(w, st.BoldFail("not ready: a gate built from this setup will not work as configured"))
	case r.Partial:
		fmt.Fprintln(w, st.Warn("partial: nothing failed, but part of the setup was not checked (CRL status or the gate's recorded policy); this is not a go-live result"))
	default:
		fmt.Fprintln(w, st.BoldOK("ready: every check passed (within what a pre-flight can check; see below)"))
	}
	fmt.Fprintln(w, "\nnot checkable ahead of time:")
	for _, n := range r.NotCheckable {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

// UsageError is a mistake in the options; the command exits with ExitUsage.
type UsageError struct{ Msg string }

// Error implements error.
func (e *UsageError) Error() string { return e.Msg }

// IOError is a file that could not be read or a tool that could not run; the command exits with ExitIO.
type IOError struct{ Err error }

// Error implements error.
func (e *IOError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying error.
func (e *IOError) Unwrap() error { return e.Err }

func short(h [32]byte) string { return fmt.Sprintf("%x", h[:4]) }

func join(s []string) string { return strings.Join(s, ", ") }
