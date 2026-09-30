package agent

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// A failed tool call's error text becomes durable content: it is journaled as the call's result,
// sent to the model, and hashed into the audit trail, where a proof can disclose it. Tools commonly
// return errors that quote a URL (net/http's *url.Error quotes the whole request URL), and a URL
// can carry a credential in its query string (an API key parameter), its userinfo, or its fragment
// (an OAuth token). So the text journaled for a failed call has every URL's userinfo, query
// values, and fragment replaced with REDACTED, keeping the scheme, host, path, and query
// parameter names, so the model still learns what was called and why it failed. A redactor set
// with WithToolErrorRedactor chooses the text first; URL redaction applies to what it returns.

// redacted replaces each credential-bearing part of a URL.
const redacted = "REDACTED"

// urlPattern matches a URL written into text: a scheme, "://", and every character up to
// whitespace, a quote, or an angle bracket.
var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>]+`)

// toolErrorText is the text journaled, and sent to the model, for a tool call that failed with
// err: redact(tool, err) if a redactor is set, else err's text with any *url.Error's URL
// redacted, and in either case with every URL in it redacted (see redactURLs).
func toolErrorText(redact func(tool string, err error) string, tool string, err error) string {
	if redact != nil {
		return redactURLs(redact(tool, err))
	}
	text := err.Error()
	// A *url.Error prints its URL quoted, and a URL can hold characters (a space, a quote) that
	// end a URL found by scanning. Print it again with its URL redacted as a whole.
	var ue *url.Error
	if errors.As(err, &ue) {
		text = strings.ReplaceAll(text, ue.Error(), fmt.Sprintf("%s %q: %v", ue.Op, redactURL(ue.URL), ue.Err))
	}
	return redactURLs(text)
}

// RedactURLs returns s with every URL in it redacted: its userinfo, each query parameter's value,
// and its fragment become REDACTED, and its scheme, host, path, and query parameter names are
// kept. It is the redaction the agent applies to a failed tool call's text; use ToolCall.ErrorText
// for a tool error, and this for other error text that can quote a URL.
func RedactURLs(s string) string { return redactURLs(s) }

// redactURLs returns s with every URL in it redacted (see redactURL). Punctuation that ends a
// sentence or closes a parenthesis just after a URL is kept out of it.
func redactURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, func(u string) string {
		trimmed := strings.TrimRight(u, ".,;:!?)]}")
		return redactURL(trimmed) + u[len(trimmed):]
	})
}

// redactURL replaces raw's userinfo with REDACTED, the value of each query parameter with
// REDACTED (keeping its name), and its fragment with REDACTED. It works on the text as written,
// without parsing it, so a URL no parser accepts is redacted all the same.
func redactURL(raw string) string {
	rest := raw
	var b strings.Builder
	if i := strings.Index(rest, "://"); i >= 0 {
		b.WriteString(rest[:i+3])
		rest = rest[i+3:]
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		b.WriteString(redacted)
		rest = rest[at:]
	}
	fragment := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		rest, fragment = rest[:i], "#"+redacted
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		params := strings.Split(rest[i+1:], "&")
		for j, p := range params {
			if name, _, ok := strings.Cut(p, "="); ok {
				params[j] = name + "=" + redacted
			} else if p != "" {
				params[j] = redacted
			}
		}
		rest = rest[:i+1] + strings.Join(params, "&")
	}
	b.WriteString(rest)
	b.WriteString(fragment)
	return b.String()
}
