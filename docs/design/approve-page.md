# Design: a browser page for approving a call

Status: proposed, for review. Nothing in this note is built. It follows the approval intake
(`approvals.Handler`, [Taking decisions over HTTP](../guides/hitl-approval.md#taking-decisions-over-http)),
which already takes a signed decision over HTTP and shows a pending call. The page is the client that
lets an approver who has no `bide` binary sign one. This revision folds in an adversarial review of
the first draft; the points it changed are marked as such.

## Goal

An approver opens a link from a notification, sees exactly what they are about to approve, and signs
the decision in the browser, with a key that never leaves it. The application records it through the
intake and signs the audit log as it does for any decision. Non-goals: login, sessions, a server-held
key for the approver, an approve link that decides on a click, a design for roles or delegation.

## The central problem: who serves the script

A signature is only as good as the code that asks for it. If the application that runs the gated
tool also serves the page the approver signs in, the application (or anyone who can change what it
serves) can serve a script that displays one call and signs another, or signs anything the approver's
key will sign. The endpoint's check does not help: the signature is valid, made by the approver's
real key, over whatever the script chose. The browser key being non-extractable stops the key leaving
the browser, not a script on the page using it. So the first draft's design (the app serves the page
from its own origin) puts the approver's trust back in the party the approval is meant to constrain.
(Review point 1.)

The design therefore has two deployment modes, and says plainly what each gives:

1. **An approver-controlled origin (the design).** The page is a static bundle (one HTML file, one
   script, no dependencies) built reproducibly from this repository and hosted by the approver's own
   organization, on a dedicated origin, separate from the application and from anything else the
   organization serves. It talks to the application's intake over CORS (`approvals.WithCORS(origin)`,
   an exact origin, never `*`). The organization pins the bundle: it publishes the bundle's SHA-256
   (in the release's signed checksums), serves it with Subresource Integrity on the entry point,
   and reviews an update before deploying it. The application cannot change what the page does.
   Compromise of that origin is the residual risk, and it is the organization's, not the gated
   application's.
2. **Served by the application (`approvals.WithPage()`, off by default, for development and
   low-assurance use).** The page is embedded in the package and served by the handler. It has the
   weaker property above: the application can serve any script. The guide states this in the option's
   documentation and the page shows a banner naming the origin it was served from, so an approver
   can see it is the application's. It must not be the default, and it is not offered as a bank
   deployment.

Neither mode makes the browser a stronger signer than `bide approve` with a key in an HSM. The page
is for approvers who would otherwise not sign at all, and its documentation says so.

## What the page must guarantee

1. **What is signed is what is shown.** The signature binds the SHA-256 of the canonical arguments
   (`agent.ApprovalDecisionBytes`). The page computes that digest itself from the arguments it
   displays; it does not sign the server's `args_sha256`. If it did, a server could show arguments A
   and supply the digest of B. The page shows the digest it computed and compares it with
   `args_sha256` as a check on the endpoint, not as the thing signed. It signs the subject it
   captured at display time, and refuses to sign if a fetch after the confirmation differs in the
   tool, the arguments or the gate, exactly as `bide approve -to` does.
2. **The key is not extractable.** An Ed25519 key pair is generated with WebCrypto
   (`extractable: false` for the private key), stored as a `CryptoKey` in IndexedDB, and used only to
   sign.
3. **A notification links only to a read-only page.** The link carries a run id and a call id and
   nothing else. Opening it decides nothing.
4. **Nothing the endpoint trusts the page for.** The endpoint verifies the signature against the
   recorded call and the enrolled key whichever client produced it.

## Flow

1. First use: the page generates the key pair, stores the private `CryptoKey`, and shows the public
   key as `ed25519:<hex>` and its fingerprint (see "Enrolling a key"). Nothing is signed until it is
   enrolled, since the endpoint refuses an approver with no key.
2. The link opens the page with `#run=&call=` in the fragment, so the ids are not sent in a
   `Referer` or logged. The page fetches `GET /pending`.
3. It renders the call: run, tool, **all** of the arguments, the gate, and the digest it computed.
   It does not enable signing if the call is not `waiting`, the approver is not on the gate, or the
   approver already has a counted decision.
4. The approver chooses Approve or Reject (Reject says REJECT) and types their approver id. The page
   signs `ApprovalDecisionBytes` for the displayed subject, with `decided_at` from the browser clock,
   and POSTs to `/decisions`.
5. It shows the gate's standing from the response, and says so when the decision does not count
   (`counted: false`). A replay is a no-op.

## Controls and what they are not

The typed id, the Approve/Reject choice, and a scroll-to-the-end requirement for long arguments are
**interface aids, not security controls**: a script running on the page can perform each of them.
They defend against a hurried or mistaken approver, not against a hostile script. The real defences
against a hostile script are the origin (above) and, stronger, a per-signature hardware gesture
(next). The page and the guide must not describe the confirmation steps as protection against
compromise. (Review point 4.)

**Per-signature gestures: WebAuthn.** A passkey asks for a user gesture (touch, biometric) for each
signature, so script on the page cannot sign silently, and the key lives in hardware. A WebAuthn
signature is over `authenticatorData || SHA-256(clientDataJSON)` with the decision bytes' digest as the
challenge, not over the decision bytes, so the gate, `agent.TallyApprovals` and `audit.VerifyApprovals`
need a new scheme (`webauthn-es256`) whose verifier checks that structure and the challenge, and an
offline verifier path for auditors. That is a protocol change with its own design; this note
recommends it as the follow-up that makes the browser a strong signer, and builds the Ed25519 page
first only because it needs no protocol change.

## Canonical arguments in the browser

The digest is over the canonical arguments: the arguments parsed as JSON and re-serialized with object
keys sorted, no insignificant whitespace, no HTML escaping, and number literals kept verbatim.
JavaScript's `JSON.parse` loses number spelling (`1.0`, `1e3`, integers past 2^53), so the page needs
its own small parser that keeps number text. A divergence from Go's canonicalization either fails a
valid signature (safe) or lets two different arguments canonicalize alike (not). Pin it:

- **Key order is by UTF-8 bytes**, the order Go sorts object keys in. JavaScript sorts strings by
  UTF-16 code units, which orders supplementary-plane keys (U+10000 and above) differently from
  U+E000 to U+FFFF. The page sorts by the keys' UTF-8 encodings. (Review point 3.)
- **Golden vectors**, generated by Go (`agent/testdata`) and consumed by both the Go tests and the
  page's tests, include: supplementary-plane keys beside U+E000 to U+FFFF keys, duplicate keys (Go's
  rule, whatever it is, pinned), lone surrogates (`\ud800` escapes), invalid UTF-8 in the source
  bytes, numbers in every spelling, nested arrays, and empty and non-JSON arguments. Anything the
  page cannot canonicalize exactly as Go does makes it refuse to sign and say so; it never falls back
  to a best effort.
- A fuzz target compares the two on generated inputs. The page's tests run under Node's `node:test`
  (Ed25519 is in `crypto.subtle`), with no npm dependency.

## Display that cannot hide content

As in the CLI: all of the arguments, in a monospace block set with `textContent` (never `innerHTML`),
with control characters, bidirectional overrides and zero-width characters shown visibly as escapes.
Very long values are shown in full behind a scroll. Signing is disabled until the approver has
reached the end, as an aid against signing what was not read (see "Controls").

## Same-origin, locked down

The page's HTML carries `Content-Security-Policy: default-src 'none'; script-src 'self'; connect-src
<the intake origin>; style-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`,
no inline script, no third-party code, `Referrer-Policy: no-referrer`, and `Cache-Control: no-store`.
It is a static file with no templating of server data into script.

**A dedicated origin is required.** IndexedDB, and the key in it, are scoped to the origin. A page
on a shared origin (an organization's general portal, or the application's own origin) shares the key
and the storage with every script on that origin, so a cross-site scripting flaw anywhere on it can use
the approver's key. The page must be served from an origin, or a subdomain, that serves nothing else
and holds no other cookies or storage. (Review point 2.)

## Enrolling a key, and rotating it

The page generates the key in the browser, so enrolment is the weakest step: the public key reaches
whoever maintains `-approver-keys` through some channel, and a key substituted in transit is a key an
attacker controls enrolled under the approver's id. (Review point 5.) So:

- The page shows the key's fingerprint (`audit.KeyID`) as a short, grouped string, and the approver
  reads it to the enrolling party **out of band** (a call, in person, a ticket in an authenticated
  system), who compares it with the fingerprint of the key they enrol before enrolling. The guide
  says an enrolment without this comparison is not an enrolment of that person.
- A **rotation** (a new browser, a cleared store, a lost device) is a new key under the same id. It
  needs a second person's sign-off, recorded where the organization records enrolments, and the old
  public key is kept: decisions made under it still verify, and an auditor needs it. The gate asks the
  application's resolver for the key, so a rotation is a change to that resolver; the note's
  recommendation is that the resolver can return, for an approver id, the key valid at the time a
  decision was recorded (a rotation-window verifier), so that a decision signed under the old key
  before the rotation still counts and one signed under it after does not. That is a small addition to
  the verifier interface and belongs with the WebAuthn design if it is not built first.
- Browser storage can be cleared by the user, by storage pressure, and, in Safari, after seven days
  without a visit for a site that is not an installed web app. The page says so at first use. A lost
  key is a rotation.
- **Two devices.** A seat is a key and an approver id holds one. A second device is a rotation, or a
  second approver id on the gate.

## What the browser key does not protect against

- Script on the origin using the key (see "The central problem" and "Controls"): the dedicated origin,
  the CSP and the pinned bundle are the defence; a WebAuthn gesture is the stronger one.
- Browser or device compromise, which has the key whether or not it is extractable.
- A hostile enrolment (above).

## GET /pending needs authorization wherever the page is served

`GET /pending` shows a call's arguments to any caller the handler lets through. With a page in front
of it, the endpoint is reachable from a browser, so anyone who can open the page's link can read the
call, and a link in a notification is forwarded and logged. Where the page is served, `WithAuth` is
required, not optional: the page will not start (`approvals.WithPage` and `WithCORS` return an error
without `WithAuth`), and the guide says the hook should tie the caller to an approver on the gate, so
that an approver reads only calls they can decide. (Review point 6.)

## Alternatives considered

- **A server-held key per approver** (the page logs in and the server signs) would make the
  application the signer of approvals, which defeats the property that the approver's signature is
  independent evidence. Rejected.
- **Approve links in notifications** (a click decides). Rejected, as the
  [notifications guide](../guides/notifications.md) says: a link that decides on a click is a
  phishing target.
- **The application serves the page by default.** Rejected for the reason in "The central problem":
  it is acceptable only as a clearly labeled low-assurance mode.

## Build plan

1. Golden vectors for canonical arguments, generated by Go (`agent/testdata`) and pinned by a Go test
   (this also pins the encoding for any other language's verifier), with the cases listed above.
2. The page: UTF-8-ordered canonicalizer, WebCrypto key handling, display, POST, the second-fetch
   check. Node tests against the vectors and the real handler.
3. `approvals.WithCORS` and `approvals.WithPage` (both requiring `WithAuth`), the headers above, and a
   test that every page response carries them.
4. A reproducible build of the bundle, with its SHA-256 in the release's signed checksums.
5. Docs: the guide section (with the two modes, the controls note, enrolment and rotation) and
   KNOWN-LIMITATIONS.

## Decisions for review

1. **Build the page, with the approver-controlled origin as the design?** The app-served mode is then
   a labeled low-assurance option. Recommendation: yes. The alternative is to build only the
   app-served page, which is simpler and weaker in the way described above, and not to describe it as
   suitable for a bank.
2. **Is Ed25519-in-browser acceptable as the first version, with WebAuthn as the named follow-up?**
   Recommendation: yes, because the Ed25519 page needs no protocol change; commit to the WebAuthn
   design next, since it is the only option that stops a hostile script signing silently.
3. **Sign the digest the browser computes (a second canonicalizer, UTF-8-ordered, with golden
   vectors)?** Recommendation: yes; the alternative breaks "what you see is what you sign".
4. **Require `WithAuth` wherever the page or CORS is enabled?** Recommendation: yes, as a
   construction error rather than a documented caution.
5. **Enrolment: require an out-of-band fingerprint comparison and a second person's sign-off for
   rotation, and add a rotation-window verifier?** Recommendation: the comparison and the sign-off are
   process, written into the guide now; the rotation-window verifier is a code change, scoped with
   WebAuthn. Say if you want it earlier.
6. **Ids in the URL fragment, and one device per approver id for the first version?** Recommendation:
   yes to both.
