# The docs' browser screenshots

`scripts/shots.sh` regenerates them, from real runs, in a real Chrome. Nothing is mocked or edited
(the two crops, the timeline and the gate-expired and identity pages, only cut a real page to the
part the guide talks about).

| Group | Script | Images (docs/guides/img) |
|---|---|---|
| `viewer` | `shots.sh` | `run-viewer-run-list`, `run-viewer-flow-halted`, `run-viewer-config-risk`, `run-viewer-config-assert` (dark) |
| `approval` | `shots-approval.sh` | `run-viewer-agent-halted`, `run-viewer-agent-verified`, `run-viewer-timeline`, `run-viewer-share` (dark) |
| `deadline` | `shots-deadline.sh` | `run-viewer-deadline-attention`, `run-viewer-gate-expired` (dark and light) |
| `identity` | `shots-identity.sh` | `run-viewer-identity-attention`, `run-viewer-identity-run` (dark and light) |
| `flows` | `shots-flows.sh` | `flow-gate-waiting`, `flow-gate-approved`, `flow-diff-strengthened`, `flow-diff-weakened`, `run-viewer-config-problem` (dark) |
| `editor` | `shots-edit.sh` | `flow-edit-weakened`, `flow-edit-save` (light, 1280 x 900) |
| `editor-typed` | `shots-edit-typed.sh` | `flow-edit-typed`, `flow-edit-typed-blocks`, `flow-edit-typed-uncertified`, `flow-edit-typed-save` (light, 1280 wide: bide edit -blocks) |
| `themes` | `shots-themes.sh` | `themes-light`, `themes-dark`, `themes-high-contrast`, `themes-acme-teal` (one page in each theme, 1200 x 900; the last is the custom theme `browser-shots/acme-teal.json`) |
| `approve-page` | `browser-shots/browser-shots.sh` (`ONLY=approve`) | `approve-page-waiting`, `approve-page-signed` (dark and light, 2x) |
| `needs-attention` | `browser-shots/browser-shots.sh` | `run-viewer-needs-attention` (dark and light, 2x) |
| `live` | `live.sh` | `run-viewer-live-dark.gif` |

`browser-shots/browser-shots.sh` also makes the Slack relay images (`ONLY=relay`), which `scripts/shots.sh`
leaves alone. `cli-*.png` and `refund-*.png` are terminal images, made by `cli.sh` and `record.sh`.

## When

Regenerate before each release, as part of the final docs pass: the viewer, the editor and the themes
change between releases, and a picture that was right a month ago is not a promise. Run
`scripts/shots.sh`, look at each image, and commit the ones that changed with the release's docs.

## One at a time

The deadline and identity demos share the fixed work directory `/tmp/bide-demo` (and each empties it), so run them
one at a time, as `scripts/shots.sh` does, and not two runs on one machine at once.

## Checks

`scripts/shots.sh` ends by comparing each image it wrote with the committed one and fails if the
dimensions changed or the file shrank by more than half (a blank page or an error page is a few kilobytes);
look at the image, then `SHOTS_ALLOW_CHANGE=1 scripts/shots.sh` accepts a change that is real. Look at
every image before committing it: the check cannot tell a wrong page from a right one that is smaller.

## How a picture is taken

One program, `browser-shots/capture.mjs`, takes the viewer's and the flow pictures. The others have
their own because they do something more: the editor pictures drive the page (`edit-shots.mjs`), the share
card is the file the page's own button saves (`share.mjs`), the GIF's frames wait while a run lands
(`live-capture.mjs`), and the Needs attention list is taken at 2x with the approve page and the relay
(`shot.mjs`, `browser-shots.sh`). The viewer and flow pictures (`capture.mjs`, `shot.mjs`) fail on a page that is not the one meant: a
response that is not 2xx, a page with no `<main>`, a run page that does not name its run. The others fail
when the element they drive is missing (the share button, the editor's forms, a run that never reaches
"started"). The
page is shown in the named color scheme (`prefers-color-scheme`) with `prefers-reduced-motion:
reduce`, so the graph's flowing dashes and the live pulse stand still; the window is 1200 pixels
wide (900 for the live GIF, 1280 for the editor) at 1x, scrollbars are hidden, and the program waits for
the network and the fonts to settle before it photographs. The height of each picture is the one
its script names, so the guide's image sizes do not move.

The data is fixed where it can be: `bide sample` writes the same runs on every call, and the
examples' flows and configs are files in the repository. The scripts refuse to start if a port they need is in use, and stop if the server they started exits,
so a stale server can never answer for it. The Needs attention pictures of the deadline and identity
demos show their database path, `/tmp/bide-demo/…`, a fixed neutral directory made for the run (mode
700, only if nothing is there or it is yours) and not a random one.

## What is not frozen

- **Wall-clock times.** Pages that run the examples live (the approval and flow runs, the deadline and
  identity demos) show the time of the run, and ages ("14d ago") move with the date you run it. A new
  picture differs from the last one in those digits.
- **Fonts and Chrome.** The pages use the system font stacks, so the machine's fonts decide the
  letterforms; the images in the repository were taken on macOS (Apple's system fonts) with Chrome
  for Testing 152. A different Chrome or system moves text by a pixel.
- **The Chrome the scripts find.** `CHROME_BIN` first, then a Chrome for Testing that puppeteer
  installed, then the usual paths. Set `CHROME_BIN` to pin it.

Install: the first run installs puppeteer-core and its dependencies from
`browser-shots/package-lock.json` with `npm ci --ignore-scripts`; no browser is downloaded.
