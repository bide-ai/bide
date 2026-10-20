# Browser images

See [../SHOTS.md](../SHOTS.md) for every screenshot script and `scripts/shots.sh`, which runs them all. This directory has the programs they share (`capture.mjs`, `share.mjs`, `live-capture.mjs`, `edit-shots.mjs`, `edit-typed-shots.mjs`, `themes-shots.mjs`, and the custom theme `acme-teal.json` the themes pictures use) and the approve page and Slack relay images.

`browser-shots.sh` regenerates five of the docs' images, each as `-dark.png` and `-light.png` at 2x,
from real runs in a real Chrome. Nothing is mocked or edited.

| Image | What it is |
|---|---|
| `approve-page-waiting`, `approve-page-signed` | The approve page against a real intake (`intake/main.go`): an agent paused at a `refund` tool gated 2 of finance, ops and risk, finance already approved. The driver (`approve.mjs`) creates the ops key in the page, the intake enrols it, then Approve is pressed. |
| `run-viewer-needs-attention` | `bide view`'s Needs attention list over the journals `bide sample` writes. |
| `notify-relay-slack-approval`, `notify-relay-slack-resolved` | The Slack payloads `bide notify relay` delivers, captured by a local stub (`stub.mjs`) that stands in for Slack and PagerDuty, and drawn by `relay.mjs` as a Block Kit style preview. The image says it is a render of the payload and not a screenshot of Slack, and carries no Slack branding. The events come from `bide notify test`, so they show that sample's values. |

```sh
assets/demo/browser-shots/browser-shots.sh                       # into docs/guides/img
OUT=/tmp/shots CHROME_BIN=/path/to/chrome assets/demo/browser-shots/browser-shots.sh
```

It needs Go, Node 20 or later (Ed25519 in WebCrypto), curl and a Chrome or Chromium; `CHROME_BIN`
names it, and it otherwise looks for Google Chrome, `google-chrome-stable`, `chromium` and a
puppeteer-installed Chrome for Testing. The first run installs puppeteer-core from the lockfile
(`npm ci --ignore-scripts`). It listens on 127.0.0.1 ports 8631, 8641 and 8642, and on a free port for the
intake, and writes only to `OUT`.

The approve page is served by the application (`approvals.WithPage`), so the page's own warning
about that is in the images. The timestamps in the viewer image come from `bide sample`'s fixed
scenario; the ages ("14d ago") move with the date you run it.
