package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bide-ai/bide/notify"
)

// Slack limits the message fits within (Block Kit).
const (
	slackHeaderMax  = 150
	slackSectionMax = 3000
	slackFieldMax   = 2000
	slackFallback   = 3000
	slackURLMax     = 3000
)

// SlackConfig configures a Slack sink.
type SlackConfig struct {
	// WebhookURL is the Slack incoming webhook URL. It is a credential: keep it in the relay's
	// configuration, and it is never put in an error or a log line. Required.
	WebhookURL string
	// ViewURL, if set, is the base of the run viewer, used for the "Open in viewer" button when the
	// notification carries no view.url of its own (bide's WithViewURL). The button is the base, a
	// slash and the escaped run ID.
	ViewURL string
	// HTTPClient, Attempts, Timeout and Backoff tune the outbound call; the zero values mean
	// http.DefaultClient-like defaults, DefaultAttempts, DefaultRequestTimeout and DefaultBackoff.
	HTTPClient *http.Client
	Attempts   int
	Timeout    time.Duration
	Backoff    time.Duration
}

// Slack is a Sink that posts a Block Kit message to a Slack incoming webhook.
type Slack struct {
	out     *outbound
	viewURL string
}

// NewSlack returns a Slack sink. The URL must be https (http is allowed for localhost, for tests).
func NewSlack(c SlackConfig) (*Slack, error) {
	out, err := newOutbound("slack", c.WebhookURL, c.HTTPClient, c.Attempts, c.Timeout, c.Backoff)
	if err != nil {
		return nil, err
	}
	if c.ViewURL != "" {
		if u, err := url.Parse(c.ViewURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("relay: slack: ViewURL needs an http or https URL")
		}
	}
	return &Slack{out: out, viewURL: c.ViewURL}, nil
}

// Name is "slack".
func (s *Slack) Name() string { return "slack" }

// Send posts p as a message.
func (s *Slack) Send(ctx context.Context, p notify.Payload) error {
	body, err := json.Marshal(SlackMessage(p, s.viewURL))
	if err != nil {
		return fmt.Errorf("relay: slack: encoding the message: %w", err)
	}
	return s.out.post(ctx, body)
}

// A SlackMsg is the JSON body of an incoming-webhook post: a plain-text fallback (what a
// notification or a screen reader shows) and Block Kit blocks.
type SlackMsg struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type     string      `json:"type"`
	Text     *slackText  `json:"text,omitempty"`
	Fields   []slackText `json:"fields,omitempty"`
	Elements []any       `json:"elements,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackButton struct {
	Type string    `json:"type"`
	Text slackText `json:"text"`
	URL  string    `json:"url"`
}

// SlackMessage formats p as a Slack message: a header naming what happened, the generated summary,
// the run, kind, deadline and (for an approval) who may decide, and a button to the run viewer when
// there is a link: the payload's view.url, else viewURL plus the run ID. Every value that came from
// the run (its ID, tool and approver names) is escaped, so none of it can open a mention
// (<!channel>) or a link.
func SlackMessage(p notify.Payload, viewURL string) SlackMsg {
	title := KindTitle(p.Kind)
	fields := []slackText{
		mrkdwn("*Run*\n" + code(p.RunID)),
		mrkdwn("*Kind*\n" + code(p.Kind)),
	}
	if p.DeadlineAt != nil {
		fields = append(fields, mrkdwn(fmt.Sprintf("*Deadline*\n<!date^%d^{date_short_pretty} {time}|%s>", p.DeadlineAt.Unix(), p.DeadlineAt.UTC().Format(time.RFC3339))))
	}
	if a := p.Approval; a != nil {
		fields = append(fields, mrkdwn("*Tool*\n"+code(a.Tool)))
		if a.Policy.Of > 0 {
			line := fmt.Sprintf("*Approvals*\n%d of %d needed, %d given", a.Policy.Need, a.Policy.Of, a.Approved)
			fields = append(fields, mrkdwn(line))
		}
		if len(a.Approvers) > 0 {
			fields = append(fields, mrkdwn("*Approvers*\n"+escapeSlack(strings.Join(a.Approvers, ", "))))
		}
	}
	if s := p.Stop; s != nil {
		scope := s.Scope
		if s.Name != "" {
			scope += " " + s.Name
		}
		fields = append(fields, mrkdwn("*Scope*\n"+code(scope)))
		who := s.By
		if !s.Signed {
			who += " (asserted)"
		}
		fields = append(fields, mrkdwn("*Set by*\n"+escapeSlack(who)), mrkdwn("*Mode*\n"+code(s.Mode)))
		if s.Reason != "" {
			fields = append(fields, mrkdwn("*Reason*\n"+escapeSlack(s.Reason)))
		}
	}
	if p.Principal != nil && p.Principal.OnBehalfOf != "" {
		fields = append(fields, mrkdwn("*On behalf of*\n"+escapeSlack(p.Principal.OnBehalfOf)))
	}
	for i := range fields {
		fields[i].Text = clip(fields[i].Text, slackFieldMax)
	}
	summary := mrkdwn(clip(escapeSlack(p.Summary), slackSectionMax))
	blocks := []slackBlock{
		{Type: "header", Text: &slackText{Type: "plain_text", Text: clip(title, slackHeaderMax)}},
		{Type: "section", Text: &summary, Fields: fields},
	}
	if link := viewLink(p, viewURL); link != "" {
		blocks = append(blocks, slackBlock{Type: "actions", Elements: []any{
			slackButton{Type: "button", Text: slackText{Type: "plain_text", Text: "Open in viewer"}, URL: link},
		}})
	}
	if p.View.Command != "" {
		// Backticks in a run ID would end the code span early.
		blocks = append(blocks, slackBlock{Type: "context", Elements: []any{
			mrkdwn("To look: `" + strings.ReplaceAll(escapeSlack(p.View.Command), "`", "'") + "`"),
		}})
	}
	return SlackMsg{Text: clip(title+": "+p.Summary, slackFallback), Blocks: blocks}
}

// viewLink is the viewer URL for p, or "" if there is none a button can carry.
func viewLink(p notify.Payload, base string) string {
	link := ""
	switch {
	case p.View.URL != nil && *p.View.URL != "":
		link = *p.View.URL
	case base != "" && p.RunID != "":
		link = strings.TrimRight(base, "/") + "/" + url.PathEscape(p.RunID)
	}
	if u, err := url.Parse(link); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(link) > slackURLMax {
		return ""
	}
	return link
}

func mrkdwn(s string) slackText { return slackText{Type: "mrkdwn", Text: s} }

// code wraps v as inline code, with the characters Slack reads in any text escaped and the one that
// would end the span replaced.
func code(v string) string {
	if v == "" {
		return "-"
	}
	return "`" + strings.ReplaceAll(escapeSlack(v), "`", "'") + "`"
}

// escapeSlack escapes the three characters Slack's mrkdwn treats as control: & < >. That is what
// stops a run ID or a tool name from becoming a mention (<!channel>, <@U123>) or a link.
func escapeSlack(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// clip cuts s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}

var kindTitles = map[string]string{
	"approval_pending":     "Approval pending",
	"approval_expired":     "Approval expired",
	"outcome_unknown":      "Run halted: outcome unknown",
	"run_aborted":          "Run aborted",
	"recovery_failing":     "Recovery failing",
	"notification_overdue": "Notification overdue",
	"resolved":             "All clear",
	"escalated":            "Escalated: not acknowledged or decided in time",
	"stop_set":             "Agents stopped",
	"stop_lifted":          "Stop lifted",
}

// KindTitle is the short heading for a notification kind; a kind it does not know is shown as it is
// with spaces for underscores.
func KindTitle(kind string) string {
	if t, ok := kindTitles[kind]; ok {
		return t
	}
	if kind == "" {
		return "bide notification"
	}
	return strings.ReplaceAll(kind, "_", " ")
}
