# Notifications: tell a person when a run needs them

A run that halts on an unknown outcome or waits at an approval gate stops until a person acts. The
notifier tells that person. `agent.WithNotifier` raises the event and keeps the delivery record in an
audited trail (see the [design note](../design/notifier.md) for the guarantee); this page is about the
shipped delivery, package `notify`: a signed HTTPS POST of a small JSON document, and the one-call
check for the receiver.

```go
import "github.com/bide-ai/bide/notify"
```

The package is stdlib only.

## Wire a webhook

<!-- docsnip: setup journal *agent.Journal; model agent.Model; store audit.EventStore; returns error -->
```go
hook, err := notify.NewWebhook(os.Getenv("BIDE_WEBHOOK_URL"),
	notify.WithSecrets(os.Getenv("BIDE_WEBHOOK_SECRET"), os.Getenv("BIDE_WEBHOOK_SECRET_PREVIOUS")),
	notify.WithTimeout(10*time.Second), // per request; this is the default
	notify.WithViewURL("https://bide.example.com/runs"), // optional: puts a view link in the payload
	notify.WithViewDB("/var/lib/orders/run.db"),         // optional: the journal the view command opens
)
if err != nil {
	return err
}

a, err := agent.New(model, journal, agent.WithNotifier(hook, audit.NewNotifyTrail(store)))
```

`NewWebhook` returns `agent.ErrConfig` for a URL that is not `https` (`http` is allowed only for
`localhost`, `127.0.0.1` and `[::1]`, for development) and for a missing secret. Secrets are
arguments, read from your configuration or environment. They are never written to the journal, the
trail or a payload, and never appear in an error.

The same option goes to `agent.RecoverLoop`, which is what catches an event whose process died before
the first record of it.

## The request

`POST <url>` with `Content-Type: application/json` and three headers:

| Header | Value |
|---|---|
| `Bide-Delivery` | The notification ID, the same on every attempt of an event, and equal to the payload's `id`. The signature covers the body, not headers, so `Verify` requires this header to be present and equal to the signed `id`. Dedupe on the verified body `id`. |
| `Bide-Timestamp` | Unix seconds when this request was sent. |
| `Bide-Signature` | `v1=<hex>`, where hex is HMAC-SHA256 of `Bide-Timestamp + "." + body` under a secret. One `v1=` entry per secret, separated by spaces. |

The signature covers the timestamp, so a captured request cannot be replayed with a fresh one. It is
always on, over every transport.

Delivery is at least once. The notifier core runs up to 3 calls per round with its own backoff, and each
`Notify` call makes one request with a per-request timeout (10 s by default) that also honors the
deadline the core gives the call. A 429 or 5xx with `Retry-After` is waited out (capped at 60 s, never
past that deadline) before the call returns, so the next call does not arrive early. A caller that uses
the webhook without the core can set `notify.WithRequestsPerCall(3)` for 1 s then 2 s of backoff between
requests in one call. Network errors, timeouts, 429 and 5xx are retried; any other 4xx is an error
wrapping `agent.ErrNotifyPermanent`, which ends the round (the core still tries again in a later round,
since a receiver's configuration can change). Redirects are never followed: a signed body does not go
anywhere you did not name. A response is accepted only when it is 2xx, so answer quickly and do the
work after replying.

TLS is 1.2 at the minimum and 1.3 when the receiver offers it. Each delivery opens its own connection,
so a rotated client certificate is used by the next delivery.

The environment's proxy settings (`HTTPS_PROXY`, `NO_PROXY`) apply, except when `WithServerCA` or
`WithServerPin` is set: then the webhook connects directly unless you give `notify.WithProxy`. A proxy
only tunnels the TLS session; certificates, pins and the client certificate are still checked against
the receiver, but the proxy sees the receiver's address and timing.

## Verify in your receiver

`notify.Verify` checks a request in one call:

<!-- docsnip: setup secrets []string; handle func(notify.Payload) -->
```go
http.HandleFunc("/bide", func(w http.ResponseWriter, r *http.Request) {
	if err := notify.Verify(r, secrets, 0); err != nil { // 0 means the 5 minute default tolerance
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p notify.Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil { // Verify leaves the body readable
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	handle(p) // dedupe on p.ID, the signed delivery ID, first
	w.WriteHeader(http.StatusNoContent)
})
```

`Verify` reads the body (up to 1 MiB), recomputes the signature over the raw bytes, compares in
constant time, and rejects a timestamp more than the tolerance from your clock, in either direction, and a `Bide-Delivery` header that is missing or is not the signed body's `id` (`ErrDeliveryID`). It computes one MAC per secret and looks at no more than 8 entries of the signature header, so a forged request costs the same however many entries it carries. If the body is larger than the limit it returns `ErrBodyTooLarge`, and `r.Body` still yields every byte.
It returns one of `ErrNoSignature`, `ErrBadTimestamp`, `ErrStale`, `ErrBadSignature`, `ErrDeliveryID`,
`ErrNoSecrets` or `ErrBodyTooLarge`; answer all of them with the same 4xx so a sender learns nothing about which check
failed. A receiver in another language needs only the construction in the table above.

To check a stored delivery later, as evidence, use `notify.VerifyBody` with the stored headers and body
and a tolerance wide enough for the delivery's age.

### Rotating the secret

While you rotate, the webhook signs with two secrets and a receiver that has either one verifies:

1. Deploy the sender with `WithSecrets(new, old)`. Requests now carry two signatures.
2. Update the receiver's secret list to `[new, old]`, then to `[new]` once step 1 is everywhere.
3. Drop `old` from the sender.

## What is in the payload, and what is not

Schema `bide.notification.v1`:

```json
{
  "schema": "bide.notification.v1",
  "id": "9f2c...e1",
  "key": "approval:orders-1042:call_01:0",
  "kind": "approval_pending",
  "run_id": "orders-1042",
  "event_at": "2026-10-13T09:14:05Z",
  "observed_at": "2026-10-13T09:14:07Z",
  "deadline_at": "2026-10-13T09:19:07Z",
  "attempt": 1,
  "summary": "Run orders-1042 is waiting for approval of tool refund (call call_01).",
  "step": { "op_kind": "tool", "op_id": "call_01", "tool": "refund" },
  "halt": null,
  "principal": { "on_behalf_of": "desk-7", "authority_ref": "" },
  "approval": {
    "tool_use_id": "call_01",
    "tool": "refund",
    "policy": { "need": 2, "of": 3 },
    "approvers": ["alice", "bob", "carol"],
    "approved": 0
  },
  "abort": null,
  "failing": null,
  "view": { "command": "bide view -db /var/lib/orders/run.db -run orders-1042", "url": null }
}
```

`kind` is `outcome_unknown`, `approval_pending`, `run_aborted`, `recovery_failing` or
`notification_overdue`. At most one of `halt`, `approval`, `abort` and `failing` is set, as the kind
says. `summary` is generated from the structured fields, so a chat message can post it as it is.
`notification_overdue` also carries a `reason`. New fields are additive within `v1`; ignore the ones
you do not know.

`view.command` is a `bide view` command, and `bide view` needs a store. With `WithViewDB` the command
names your SQLite journal file, as above, and runs where that path is readable. Without it the webhook
cannot know the store, so the command is a marked template, `bide view -db <journal.db> -run
orders-1042`, to fill in; it fails with a usage error if pasted as it is. A Postgres DSN is a
credential and never goes in a payload: an adapter that reads from Postgres replaces the `-db
<journal.db>` part with `-postgres "$DSN"` itself.

**In it:** kind, run ID, the step and tool name, the gate's approver IDs and its policy (k of n), the
run's principal, the times, and a view pointer.

**Deliberately not in it:**

- Error text, tool arguments, tool results and model output. Error text can embed arguments, card or
  account numbers and personal data, and a webhook reaches a wider audience than the journal.
- An approve or resolve link. The notification carries a view pointer only (a `bide view` command,
  and a URL when you set `WithViewURL`). A link that decides on a click is a phishing target, and it
  would bypass the point of the gate, where a decision is signed with the approver's key. Acting from
  a notification is a non-goal; the approver runs `bide approve` ([guide](hitl-approval.md#deciding-from-the-terminal)),
  which reads the call from the journal, not from the message.
- Any decision about who may approve. The approver list is for routing and is advisory; the gate
  counts a decision only from a listed approver with a verifying signature. The principal is the party
  the run acts for, which is not always the person who asked, so route away from it as one input and
  keep your own rule for who the requester is.

Anything more is an explicit allowlist, named field by field:

```go
notify.WithFields(notify.ErrorText, notify.Args("refund"))
```

`ErrorText` adds `error` (bounded to 4 KiB). `Args("refund")` adds `approval.args` for approval
notifications of the tools named, and of no other tool. A deployment under PCI-style rules leaves both
out and puts detail behind the viewer, where access control applies.

## Mutual TLS, a private CA and a key pin

All three are optional, and none replaces the signature. Mutual TLS answers who may connect; the
signature answers who sent this body. TLS ends at proxies and gateways, so the application often never
sees the client identity, while the signature travels with the payload; and a signature can be checked
later over the stored body, where a handshake leaves nothing to check. Most receivers (Slack,
PagerDuty) accept no client certificates at all, so the signature is the check that works everywhere.

<!-- docsnip: setup secret string -->
```go
hook, err := notify.NewWebhook("https://approvals.internal.example/bide",
	notify.WithSecrets(secret, ""),
	notify.WithClientCert("/etc/bide/client.crt", "/etc/bide/client.key"), // or WithClientCertPEM
	notify.WithServerCA("/etc/bide/receiver-ca.pem"),                        // replaces the system roots
	notify.WithServerPin("1f0e...c9a4", "77ab...02de"),                      // SPKI SHA-256, up to two
	notify.WithOnError(func(err error) { log.Println("notify:", err) }),
)
```

- **Client certificate.** Re-read when the files change: the files' modification time, size and a hash
  of their contents are checked at most once a minute, and again after a failed request and before a retry, so a rotated or short-lived
  certificate (cert-manager, SPIFFE) is picked up without a restart. A read that fails keeps the last
  good pair and reports the error to `WithOnError`; give it the function you pass to
  `agent.WithNotifyErrors`.
- **Server CA.** `WithServerCA` verifies the receiver against your bundle in place of the system roots,
  it does not add to them, so a public CA cannot vouch for your private receiver.
- **Server pin.** The SHA-256 of the receiver certificate's SubjectPublicKeyInfo (64 hex characters or
  base64; `notify.SPKIHash(cert)` computes it). The handshake fails unless a certificate in the
  verified chain matches, so a CA key pins the whole chain. Pass two while the receiver rotates its
  key. Pinning is in addition to normal verification, never instead of it.
- Revocation checking is the receiver's side.

Each accepted delivery records how it was authenticated in the `auth` field of the trail's `acked`
record: the signature scheme and which secret slot signed (`current`, or `current+previous` during a
rotation), never a secret; whether a client certificate was presented and its SPKI hash; whether the
receiver was pinned or checked against a custom CA; and the negotiated TLS version. An audit policy can
then require that approval notifications were sent with mTLS.

## Slack

An incoming webhook wants `{"text": "..."}`, and a Workflow Builder webhook wants flat string
variables. The payload is neither, and Slack cannot check a signature header, so put a small adapter in
front of it: it verifies, then forwards the generated `summary`.

<!-- docsnip: setup secrets []string; slackURL string -->
```go
http.HandleFunc("/bide-to-slack", func(w http.ResponseWriter, r *http.Request) {
	if err := notify.Verify(r, secrets, 0); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p notify.Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, _ := json.Marshal(map[string]string{"text": p.Summary + " To look: `" + p.View.Command + "`"})
	resp, err := http.Post(slackURL, "application/json", bytes.NewReader(msg))
	if err != nil || resp.StatusCode/100 != 2 {
		http.Error(w, "upstream", http.StatusBadGateway) // bide retries the round
		return
	}
	resp.Body.Close()
	w.WriteHeader(http.StatusNoContent)
})
```

A 502 makes bide retry, and a Slack post that succeeded before a lost reply may repeat: that is the
at-least-once contract. Remember `Bide-Delivery` in your adapter's store if a duplicate message matters.
The Slack webhook URL is itself a credential; keep it in the adapter's configuration, not in bide's.

## PagerDuty

The Events API v2 wants a `routing_key`, an `event_action` and a `dedup_key`, so the same adapter
shape applies. Use the notification ID as the dedup key and PagerDuty collapses redeliveries for you:

<!-- docsnip: setup p notify.Payload; routingKey string -->
```go
event := map[string]any{
	"routing_key":  routingKey,
	"event_action": "trigger",
	"dedup_key":    p.ID,
	"payload": map[string]any{
		"summary":  p.Summary,
		"source":   "bide",
		"severity": "critical",
		"custom_details": map[string]any{
			"run_id": p.RunID, "kind": p.Kind, "approvers": p.Approval, "view": p.View.Command,
		},
	},
}
```

POST it to `https://events.pagerduty.com/v2/enqueue`. For `approval_pending` you may prefer `warning`
and for `outcome_unknown` `critical`: a halt on an unknown outcome is the case where a missed alert is
expensive.

## What an acknowledgement means

The sink answering 2xx means your endpoint accepted the request, not that a person read it. The times
in the trail are the notifying process's clock; anchor the trail's tree head with a timestamp to bound
them from above.
