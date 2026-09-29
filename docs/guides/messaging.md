# Connecting a messenger (Slack, Telegram, WhatsApp, SMS, Discord)

Short version: the SDK ships no messenger connectors, and it should not. A chat transport is edge
plumbing, not accountability, so it sits on the wrong side of the non-goal line: the formats and signing schemes churn, every platform already has a
mature Go library (`slack-go`, `telebot`, `discordgo`, the Twilio and Meta SDKs), and none of it
makes an agent more provable. What the SDK owes you is a run that is safe to drive from an untrusted,
redelivering channel. That it already provides. This page shows the pattern; the runnable version is
[`examples/webhook`](../../examples/webhook/main.go).

## The core is transport-agnostic on purpose

`Agent.Run(ctx, runID, input)` takes a caller-supplied `runID` and a plain string. `Session(ctx, id)`
gives multi-turn continuity keyed by any string. A messenger integration is glue you write in your
own webhook handler:

1. Verify the provider's signature and return 200 fast (both are the handler's job, not the SDK's).
2. Map the channel's thread or conversation id to a run or a `Session` id.
3. Call `Run` / `Send`, then post the reply back over the provider's API.

Nothing here is missing from the SDK. The one part worth doing deliberately is the next section.

## Idempotency is the part that matters

Every messenger redelivers. Slack retries any event you do not acknowledge within three seconds, up
to three times; Twilio, Telegram, and Meta all retry on timeout. A handler that re-runs the agent on
a redelivery fires its side-effecting tools twice: a double ticket, a double refund, a double
outbound message. Most frameworks make you stand up a separate dedup table to defend against this.

Here it falls out of the durable journal you already have. `store.Do` is at-most-once per
`(runID, step)`: a recorded step returns its result without re-running. Key the work off the
provider's stable event id and a redelivery replays instead of re-executing. This is the same
at-most-once property the chaos benchmark proves (`maxFired=1`), now applied to an inbound channel.

### Stateless command bot

For command-style bots with no memory, key the run itself by the event id:

```go
// Slack sends a stable event_id / client_msg_id; the X-Slack-Retry-Num header marks redelivery.
runID := "msg/" + channelID + "/" + eventID
reply, err := a.Run(ctx, runID, text) // redelivery hits the same runID and replays the journal
```

A redelivered event resumes the recorded run rather than re-calling the model and re-firing tools.

### Conversational bot

For multi-turn bots, use a `Session` keyed by the conversation/thread id for memory, and send each
inbound message with `SendOnce` keyed by its event id. `Send` alone is not enough: it keys turns by
index, so a redelivery would open a second turn.

```go
sess, err := a.Session(ctx, conversationID)
if err != nil {
	return "", err
}
msg, err := sess.SendOnce(ctx, eventID, text)
if err != nil {
	return "", err // a pause/error: the redelivered event resumes the same turn
}
return msg.Text(), nil
```

`SendOnce` answers a message at most once per key. A redelivered event whose turn completed returns
the recorded answer without calling the model, even if the process died after the turn and before
the reply went out. A turn interrupted by a crash or a pause resumes when the event is redelivered,
in its own journal, so a different message arriving in between gets its own turn. Reusing a key for
different text is `ErrConfig`. Several workers may hold handles on one conversation: every
message is recorded once, and a handle that is behind catches up from the journal before
answering, so each turn sees the conversation as it stands.

The first delivery runs the turn and records the reply under the event id; a redelivery returns the
recorded reply without advancing the transcript. If the turn pauses (a tool needs approval) or
errors, nothing is recorded, so the next redelivery correctly retries it.

## Attribution for the audit trail

For the governed and audited story, carry the messenger user's identity into the run so the audit
record ties the action to who asked. Put it in the system prompt, the tool context, or a governed
input field. The run is then replayable and provable like any other, with the requester on the
record. See [AUDIT.md](audit.md) and [GOVERNANCE.md](governance.md).

## What stays your responsibility

- Signature verification (Slack signing secret, Twilio `X-Twilio-Signature`, Meta app secret).
- Acknowledging within the provider's timeout: return 200 immediately and run the agent
  asynchronously, keyed by the event id, so the reply is posted when ready and a redelivery replays.
- Outbound delivery over the provider's API, including its own rate limits.
- Mapping provider ids to your run and session ids (the two `runID` conventions above are a
  starting point, not a requirement).
