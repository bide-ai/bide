# Design: a browser page for approving a call

Status: built (the page, `approvals.WithPage`, `approvals.WithCORS`, `approvals/cmd/approvepage`, and the golden vectors); the passkey follow-up is not. The user decided the questions at the end of this note (below), and the page was built to this note. It follows the approval intake
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
   an exact origin, never `*`). A cross-origin request is preceded by an `OPTIONS` preflight that carries
   no credentials: the handler answers it before mutual TLS or bearer authentication is applied (and
   before `WithAuth`), with only the CORS headers and no data, or the browser can never reach the
   endpoint; every other request is authenticated as before. The organization pins the bundle. Subresource Integrity covers
   only a script or stylesheet a page loads, not the HTML document that loads it, so the entry
   document itself is pinned another way: the release publishes the SHA-256 of every file in the
   bundle as `approve-page.SHA256SUMS`, signed keylessly like the release's checksums, the organization verifies them when it deploys and reviews an
   update first, and the entry document is a minimal loader that carries `integrity` attributes for
   the script and stylesheet (so a changed script is refused by the browser even if the host is
   changed to serve it). A host that can change the entry document can still change what runs, which
   is why the organization, not the application, hosts it and why a hash check at deploy time, and
   not the browser, is what pins the document. The application cannot change what the page does.
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
Very long values are shown in full in a scroll box and are never clipped: a clipped view with Approve
enabled would sign arguments the approver did not read, which the CLI refuses to do. Signing is also disabled
until the approver has reached the end of the box, as an aid (see "Controls").

## Why you are being asked (as built)

When the application holds a call for a reason beyond its tool's own gate, the pending call carries it
(`approvals.Pending.Why`, `agent.GateReason`: the gate-reason record the engine wrote), and the page shows a
"Why you are being asked" list above the arguments: for the escalation gate of a tainted call, each source of
untrusted content its turn read, with where (tool and call, the run's input, retrieved documents) and the journal
position; for a gate a cap rule holds, the rule, this call's amount and the limit as decimals (the ISO 4217
exponent table is carried in `core.js`, and a Go test holds it equal to the cap rules' table), what the rule decided
and why, each labelled with the time of the ask. Two records of the run that name the call and disagree show one
warning line, "conflicting reason records", and none of their reasons.

- **Text only.** Each reason is one line set with `textContent` in a list item, with the characters that could hide
  content written as escapes (`safeText`); the page assigns no markup, and its policy (`default-src 'none'`, no
  inline script or style) is unchanged. The reader of the list is strict: an unknown member, a non-integer, a repeated member,
  a string over 512 characters or more than 64 reasons stop the page from showing the call at all, and nothing is signed.
- **The application's claim.** The list says so: the reasons are the application's record of why the call was held,
  not part of what the approver signs, and the page cannot check them. The signed bytes are unchanged
  (`bide.approval.v3`); binding the reason into a decision is a new decision format and not built.
- **No model-written prose.** The page shows no text the model wrote beyond the arguments. Everything in the arguments, the values and
  the member names, is chosen by the model (a tool's schema only constrains the choice, and the page cannot see
  the schema), so whenever there are arguments the page says so once under them: "model-written, unverified".

## What it looks like, and who can change that

The page's look is part of the pin. Its stylesheet, `app.css`, is the shipped themes (`light`, `dark`,
`high-contrast`) written in front of the page's own rules at build time, picked by the reader's system
settings alone: `prefers-color-scheme`, `prefers-contrast: more` and `forced-colors: active`. There
is no toggle and no `data-theme`, and the script, the URL, `config.json` (which names the intake and
the credentials mode and nothing else) and the host supply no theme, colour, font, name or logo, so a
deployment cannot change what an approver sees without changing a pinned file. That is a property of the
bundle: in the application-served mode (`WithPage`) the application serves the files and the headers, so
it can serve anything, and no pin applies, as the page's own banner says; a `Link:` stylesheet header, a
proxy or a service worker belongs to whoever serves the page, and the pin does not cover it. A look that an application could
choose would let it hide the digest, make REJECT look like the safe button or draw the origin line as
something else, on the one page whose point is that the approver sees what they sign. (A host that
wants its own colours forks the bundle and pins its own hash, as for any change to this page; a name
and logo slot was considered and is not built, see [Themes](../design/themes.md), option C.) The themes are
the same checked data the run viewer uses: contrast 4.5:1 for text and 3:1 for a control's edge in
every theme, a disabled button told by a dashed edge as well as its colour, and the system's colours
under forced colours. Approve and REJECT differ in shape in every mode, not only in colour: REJECT has a
heavy double edge and Approve a thin solid one, because under forced colours both take the system's button
colours and would otherwise differ by their words alone. A Chrome test (`TestPage_LookInChrome`) draws the page under each look and checks what an approver must see (shapes, sizes, opacity, labels from the DOM, an ok and a bad result), a Go
test (`TestPage_TakesNoThemeInput`) holds the no-input property, and a committed golden of the bundle's `SHA256SUMS` (`TestPage_BundleHashesAreReviewed`) makes any change to the bundle, including a change to a shipped theme, a deliberate one in review. The bundle is larger by the generated
themes, about 5 KB (`app.css` 1.5 KB to 6.9 KB).

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

## Decisions

Decided by the user, and the basis for the build:

1. **Build the page**, with the approver-controlled origin or pinned bundle as the design (the
   app-served page is the labeled low-assurance mode only).
2. **The browser computes the canonical digest itself** and signs that, with a UTF-8-ordered
   canonicalizer and golden vectors from Go.
3. **Passkeys (WebAuthn) are a separate follow-up**, with their own design and a new signature
   scheme. Until they exist, **gates for high-value actions should wait for passkeys**: a key a
   script on the page can use is not a per-signature human gesture, so the Ed25519 page suits gates
   where an approver's key held in a browser is an acceptable level of assurance, not those where each
   signature must be shown to be a deliberate act of a person. The guide says so beside the page's
   documentation, and the page's first-use text says it.
4. **Ids go in the URL fragment**, not the query.
5. **One device per approver id** for the first version; a second device is a rotation, or a second
   approver id on the gate.

Standing from the review, carried into the build: `WithAuth` is required wherever the page or CORS is
enabled (a construction error); enrolment needs an out-of-band fingerprint comparison, and a rotation
a second person's sign-off, both written into the guide as process; the rotation-window verifier is a
code change scoped with the WebAuthn design.
