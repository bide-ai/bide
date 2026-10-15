package approvecmd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

// -to <url> decides through an application's approval intake (package approvals)
// instead of writing the journal: it fetches the pending call from the endpoint, shows it, signs the
// decision with the approver's key, and posts it. The application records it through its own
// journal, so its audit log signs it as it is written. What the endpoint says is not trusted for
// what is signed: the command computes the digest of the arguments it displays and refuses an
// endpoint whose own digest differs, and the endpoint verifies the signature against the call it
// holds, so arguments shown that are not the recorded ones produce a decision that records nothing.

const (
	remoteTimeout = 30 * time.Second
	maxRemoteBody = 1 << 20
)

// remoteURL checks -to: https, or http only to a loopback address (the request carries the call's
// arguments, and a bearer token when -token-env is set), with no user info, query or fragment.
func remoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("-to %q is not a URL with a host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("-to must be the endpoint's base URL only: no user info, query or fragment (credentials go in -token-env or -client-cert)")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return nil, fmt.Errorf("-to %s is http to a host that is not this machine: the call's arguments and any token would cross the network in the clear; use https", u.Host)
		}
	default:
		return nil, fmt.Errorf("-to %q: the scheme must be https (or http to a loopback address)", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// httpClient is the client for -to: no redirects (a redirect could carry the token elsewhere), TLS
// 1.2 at least, the CA in -tls-ca instead of the system's when given, and a client certificate for
// mutual TLS when -client-cert and -client-key are.
func (c *cmd) httpClient() (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.f.tlsCA != "" {
		pem, err := os.ReadFile(c.f.tlsCA)
		if err != nil {
			return nil, fmt.Errorf("-tls-ca: %w", unwrapPath(err))
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-tls-ca %s holds no PEM certificate", c.f.tlsCA)
		}
		cfg.RootCAs = pool
	}
	if c.f.clientCert != "" {
		cert, err := tls.LoadX509KeyPair(c.f.clientCert, c.f.clientKey)
		if err != nil {
			return nil, fmt.Errorf("-client-cert and -client-key: %w", unwrapPath(err))
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:       remoteTimeout,
		Transport:     &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// remote is one conversation with an endpoint.
type remote struct {
	hc    *http.Client
	base  *url.URL
	token string
}

// do sends a request and returns the status and the body (at most maxRemoteBody).
func (r *remote) do(ctx context.Context, method, path string, query url.Values, body []byte) (int, []byte, error) {
	u := *r.base
	u.Path += path
	u.RawQuery = query.Encode()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteBody+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(b) > maxRemoteBody {
		return resp.StatusCode, nil, fmt.Errorf("the endpoint's answer is larger than %d bytes", maxRemoteBody)
	}
	return resp.StatusCode, b, nil
}

// refusedBy turns an endpoint's error answer into a refusal that says what it said.
func refusedBy(status int, b []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if audit.UnmarshalStrict(b, &e) == nil && e.Error.Code != "" {
		return refuse("the endpoint refused (HTTP %d, %s): %s", status, safe(e.Error.Code), safe(e.Error.Message))
	}
	return refuse("the endpoint answered HTTP %d, which is not a pending call or a decision", status)
}

// pending fetches the call from the endpoint and checks it is consistent: the call asked for, the
// signature format this command signs, and the arguments digest the command computes from the
// arguments shown equal to the one the endpoint states.
func (r *remote) pending(ctx context.Context, run, call string) (*approvals.Pending, agent.ApprovalSubject, error) {
	status, b, err := r.do(ctx, http.MethodGet, "/pending", url.Values{"run": {run}, "call": {call}}, nil)
	if err != nil {
		return nil, agent.ApprovalSubject{}, fmt.Errorf("fetch the pending call: %w", err)
	}
	if status != http.StatusOK {
		return nil, agent.ApprovalSubject{}, refusedBy(status, b)
	}
	var p approvals.Pending
	if err := audit.UnmarshalStrict(b, &p); err != nil {
		return nil, agent.ApprovalSubject{}, fmt.Errorf("the endpoint's answer is not a pending call: %w", err)
	}
	if p.Run != run || p.Call != call {
		return nil, agent.ApprovalSubject{}, refuse("the endpoint described run %s call %s, not the run %s call %s asked for; nothing was signed", safe(p.Run), safe(p.Call), safe(run), safe(call))
	}
	if p.DecisionFormat != agent.DecisionFormat {
		return nil, agent.ApprovalSubject{}, refuse("the endpoint's decision format is %s, and this bide signs %s; nothing was signed", safe(p.DecisionFormat), agent.DecisionFormat)
	}
	args := json.RawMessage(p.ArgsText)
	if len(p.Args) > 0 {
		if p.ArgsText != "" {
			return nil, agent.ApprovalSubject{}, refuse("the endpoint sent the arguments both as JSON and as text; nothing was signed")
		}
		args = p.Args
	}
	s := agent.ApprovalSubject{RunID: run, ToolUseID: call, ToolName: p.Tool, Args: args}
	if got := audit.ApprovalArgsDigest(s); got != p.ArgsSHA256 {
		return nil, agent.ApprovalSubject{}, refuse("the endpoint's arguments digest (%s) is not the digest of the arguments it shows (%s): it is inconsistent, and nothing was signed", safe(p.ArgsSHA256), got)
	}
	return &p, s, nil
}

// remotePreflight refuses a decision the endpoint's own report says cannot be made.
func remotePreflight(p *approvals.Pending, as string) error {
	switch p.State {
	case approvals.StateWaiting:
	default:
		return refuse("call %s of run %s is not waiting for a decision (the endpoint says it is %s)", safe(p.Call), safe(p.Run), safe(p.State))
	}
	if !slices.Contains(p.Gate.Approvers, as) {
		return refuse("%q is not an approver of the gate on call %s; its approvers are: %s", safe(as), safe(p.Call), safe(strings.Join(p.Gate.Approvers, ", ")))
	}
	for _, d := range p.Gate.Decisions {
		if d.Approver == as && d.Counted {
			return refuse("%s already decided on call %s: %s", safe(as), safe(p.Call), verdict(d.Approved))
		}
	}
	switch {
	case p.Gate.Passed:
		return refuse("the gate on call %s already has its approvals (as the endpoint counts them); nothing more is needed", safe(p.Call))
	case p.Gate.Failed:
		return refuse("the gate on call %s can no longer pass (as the endpoint counts them)", safe(p.Call))
	}
	return nil
}

// showRemote prints what is about to be signed. Everything shown came from the endpoint, and the
// digest is the one this command computed.
func (c *cmd) showRemote(p *approvals.Pending, s agent.ApprovalSubject, sg audit.Signer) (clipped bool) {
	st := c.st
	action := st.Bold("Approve this call?")
	if c.f.reject {
		action = st.BoldFail("REJECT this call?")
	}
	fmt.Fprintf(c.out, "%s %s\n\n", action, st.Dim("(read from the endpoint "+safe(c.f.to)+"; it verifies your signature against the journal's copy of the call)"))
	row := func(k, v string) { fmt.Fprintf(c.out, "  %-10s %s\n", k, v) }
	row("run", safe(s.RunID))
	row("call", safe(s.ToolUseID))
	row("tool", st.Bold(safe(s.ToolName)))
	row("gate", fmt.Sprintf("%d of %d must approve: %s", p.Gate.Need, len(p.Gate.Approvers), safe(strings.Join(p.Gate.Approvers, ", "))))
	for _, d := range p.Gate.Decisions {
		if d.Counted {
			row("decided", fmt.Sprintf("%s %s (counted by the endpoint)", safe(d.Approver), verdict(d.Approved)))
		}
	}
	row("pending", safe(strings.Join(p.Gate.Pending, ", ")))
	row("you", fmt.Sprintf("%s, with %s key %s", st.Bold(c.f.as), sg.Alg(), keyName(sg)))
	if c.f.approverKeys == "" {
		row("", st.BoldFail("UNVERIFIED")+" "+st.Warn("this key is not checked against the key enrolled for you (-unverified): if the application enrols another, the decision is refused"))
	}
	if p.AskedAt != "" {
		row("asked", safe(p.AskedAt))
	}
	text := prettyArgs(s.Args)
	hidden := 0
	if !c.f.full {
		text, hidden = clip(text)
	}
	fmt.Fprintf(c.out, "\n  arguments\n")
	for _, l := range strings.Split(text, "\n") {
		fmt.Fprintf(c.out, "    %s\n", l)
	}
	if hidden > 0 {
		fmt.Fprintf(c.out, "    %s\n", st.Warn(fmt.Sprintf("[%d more bytes not shown; the signature binds all of them; -full shows everything]", hidden)))
	}
	fmt.Fprintf(c.out, "\n  %s %s\n", "args SHA-256", st.Bold(audit.ApprovalArgsDigest(s)))
	fmt.Fprintf(c.out, "  %s\n\n", st.Dim("computed here from the arguments above; your signature binds this digest with the run, call, tool and your id, your decision and its time, and nothing else"))
	return hidden > 0
}

// approveRemote is bide approve -to for one call.
func (c *cmd) approveRemote(ctx context.Context) error {
	f := c.f
	base, err := remoteURL(f.to)
	if err != nil {
		return err
	}
	hc, err := c.httpClient()
	if err != nil {
		return err
	}
	rem := &remote{hc: hc, base: base}
	if f.tokenEnv != "" {
		if rem.token = os.Getenv(f.tokenEnv); rem.token == "" {
			return fmt.Errorf("-token-env %s: the variable is empty or not set", f.tokenEnv)
		}
	}
	var keys map[string]audit.Verifier
	if f.approverKeys != "" {
		if keys, err = approverKeys(f.approverKeys); err != nil {
			return err
		}
	}
	signer, err := c.src.Load(f.key)
	if err != nil {
		// The source's error may hold text from a vendor library: show it escaped.
		return errors.New(safe(err.Error()))
	}
	if cl, ok := signer.(io.Closer); ok {
		defer cl.Close()
	}
	if err := checkEnrolled(keys, f.as, signer); err != nil {
		return err
	}
	receiptPath, err := c.receiptTarget()
	if err != nil {
		return err
	}

	p, s, err := rem.pending(ctx, f.run, f.call)
	if err != nil {
		return err
	}
	if err := remotePreflight(p, f.as); err != nil {
		return err
	}
	shown := audit.ApprovalArgsDigest(s)
	if clipped := c.showRemote(p, s, signer); clipped {
		return refuse("the call's arguments are longer than the view, and the signature binds all of them: bide approve will not sign what you have not seen. Rerun with -full to see them all (the digest above is the one that would be signed)")
	}
	if !c.confirm(false) {
		return refuse("not confirmed; nothing was recorded")
	}
	// The gate may have been decided, or the call changed, while the approver sat at the prompt: fetch
	// again and refuse on any difference in what was shown. What is signed is the subject captured
	// at display time (s: run, call, tool, arguments), never one rebuilt from this second fetch, so
	// the approver signs exactly what they read.
	p2, s2, err := rem.pending(ctx, f.run, f.call)
	if err != nil {
		return fmt.Errorf("the endpoint changed while you were deciding; nothing was signed: %w", err)
	}
	if err := remotePreflight(p2, f.as); err != nil {
		return fmt.Errorf("the endpoint changed while you were deciding; nothing was signed: %w", err)
	}
	if s2.ToolName != s.ToolName || audit.ApprovalArgsDigest(s2) != shown || s2.RunID != s.RunID || s2.ToolUseID != s.ToolUseID ||
		p2.Gate.Need != p.Gate.Need || !slices.Equal(p2.Gate.Approvers, p.Gate.Approvers) || p2.DecisionFormat != p.DecisionFormat {
		return errors.New("the call or its gate changed while you were deciding (the tool, the arguments, or who must approve); nothing was signed")
	}

	at := now()
	msg := agent.ApprovalDecisionBytes(s, f.as, !f.reject, at)
	if !digestAt(msg, s, shown) {
		return errors.New("internal error: the decision bytes do not carry the shown arguments digest; nothing was signed")
	}
	sig, err := signer.Sign(msg)
	if err != nil {
		return fmt.Errorf("sign the decision (nothing was sent): %w", err)
	}
	d := agent.Decision{RunID: f.run, ToolUseID: f.call, ApproverID: f.as, Approved: !f.reject, DecidedAt: at, Alg: signer.Alg(), Signature: sig}
	body, err := json.Marshal(approvals.Request{Run: f.run, Call: f.call, Approver: f.as, Approved: !f.reject,
		DecidedAt: agent.FormatTimestamp(at), Alg: string(signer.Alg()), Signature: sig})
	if err != nil {
		return err
	}
	status, rb, err := rem.do(ctx, http.MethodPost, "/decisions", nil, body)
	if err != nil {
		return fmt.Errorf("the request failed, and the decision may or may not have been recorded: run the same command again to see the gate (a decision already recorded for you is refused as already decided): %w", err)
	}
	if status != http.StatusOK {
		return refusedBy(status, rb)
	}
	var resp approvals.Response
	if err := audit.UnmarshalStrict(rb, &resp); err != nil {
		return fmt.Errorf("the decision was sent, and the endpoint's answer is not a decision response (it may have been recorded): %w", err)
	}
	if !agent.IsDecisionStepName(resp.Record, f.call, f.as) {
		return fmt.Errorf("the endpoint's answer names %q as the journal record of the decision, which is not the name of a decision record of %s on %s (it may have been recorded); check the gate before relying on it", safe(resp.Record), safe(f.as), safe(f.call))
	}
	st := c.st
	how := "signed and recorded by the application"
	if !resp.Recorded {
		how = "already recorded (a replay changes nothing)"
	}
	fmt.Fprintf(c.out, "\n%s%s call %s as %s, %s (decided %s)\n", st.Pass(), c.verb(!f.reject), safe(f.call), st.Bold(f.as), how, agent.FormatTimestamp(at))
	c.remoteStanding(resp.Gate)
	if err := c.remoteReceipt(resp.Receipt, resp.Record, d, s, signer, receiptPath); err != nil {
		return err
	}
	if !resp.Counted {
		// Two decisions of one approver sent at once (an approve and a reject) are both recorded and
		// the first in journal order counts: this one is not it, and must not report success.
		why := cmp.Or(resp.Reason, "an earlier decision of yours")
		return fmt.Errorf("the decision was recorded, but it does not count: another decision of yours counts (%s)", safe(why))
	}
	return nil
}

// remoteStanding prints the gate as the endpoint counts it after the decision.
func (c *cmd) remoteStanding(g approvals.Gate) {
	fmt.Fprintf(c.out, "gate (as the endpoint counts it): %d of %d needed approved, %d denied\n", g.Approved, g.Need, g.Denied)
	switch {
	case g.Passed:
		fmt.Fprintf(c.out, "%s the gate has its approvals. The application re-runs the run, or its RecoverLoop does.\n", c.st.Pass())
	case g.Failed:
		fmt.Fprintf(c.out, "%s the gate can no longer pass; the call will be denied.\n", c.st.Cross())
	default:
		fmt.Fprintf(c.out, "Next: the other pending approvers decide%s.\n", names(": ", g.Pending))
	}
}

// remoteReceipt checks the receipt the endpoint returned against the decision just signed, and
// writes it where the local flow writes its receipt. A receipt that is not this decision, or does
// not verify under the approver's own key, is not written.
func (c *cmd) remoteReceipt(rcp *audit.DecisionReceipt, record string, d agent.Decision, s agent.ApprovalSubject, sg audit.Signer, path string) error {
	if rcp == nil {
		return errors.New("the decision was recorded, but the endpoint returned no receipt for it (it holds a record of the decision that does not verify under the key it resolves now), so none was written")
	}
	rc := *rcp
	own, err := audit.NewVerifier(sg.Alg(), sg.PublicKey())
	if err != nil {
		return fmt.Errorf("the decision was recorded; your key is not a usable verification key, so no receipt was written: %w", err)
	}
	if rc.RunID != d.RunID || rc.ToolUseID != d.ToolUseID || rc.ToolName != s.ToolName || rc.Approver != d.ApproverID || rc.Approved != d.Approved ||
		rc.DecidedAt != agent.FormatTimestamp(d.DecidedAt) || rc.Alg != d.Alg || rc.ArgsSHA != audit.ApprovalArgsDigest(s) || rc.Record != record || !agent.IsDecisionStepName(rc.Record, d.ToolUseID, d.ApproverID) {
		return errors.New("the decision was recorded, but the receipt the endpoint returned is not this decision, so none was written")
	}
	if rc.Anchor != nil {
		return errors.New("the decision was recorded, but the receipt the endpoint returned carries an anchoring proof, which only your own verify-receipts -update may add, so none was written")
	}
	// The recorded signature may differ from the one just sent (a retry of a randomized scheme's
	// decision answers from the record journaled first); it must verify under the approver's own key
	// for the same decision, which Verify checks.
	if err := rc.Verify(own); err != nil {
		return fmt.Errorf("the decision was recorded, but the receipt the endpoint returned does not verify, so none was written: %w", err)
	}
	if !bytes.Equal(rc.Signature, d.Signature) {
		fmt.Fprintln(c.out, "note: the recorded signature differs from the one just sent: the endpoint answered from the decision journaled first (a retry of a randomized scheme, such as ecdsa-p256, signs one decision into different bytes); the receipt holds the recorded signature, which verifies under your key")
	}
	if path == "" {
		path = filepath.Join(c.src.receiptDir(c.f.key), defaultReceiptName(c.f.run, c.f.call, c.f.as, rc.DecidedAt))
	}
	b, err := json.MarshalIndent(rc, "", "  ")
	if err != nil {
		return fmt.Errorf("the decision was recorded; its receipt could not be made: %w", err)
	}
	if err := writeExclusive(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("the decision was recorded, but its receipt was not written: %w", err)
	}
	fmt.Fprintf(c.out, "%sreceipt %s %s\n", c.st.Pass(), c.st.Path(path), c.st.Dim("(mode 0600; its record, position and leaf hash are the application's word until a signed head covers them: bide audit verify-receipts -update)"))
	return nil
}
