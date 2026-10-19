# The docs' browser screenshots

`scripts/shots.sh` regenerates them, from real runs, in a real Chrome. Nothing is mocked or edited
(the two crops, the timeline and the gate-expired and identity pages, only cut a real page to the
part the guide talks about).

| Group | Script | Images (docs/guides/img) |
|---|---|---|
| `viewer` | `shots.sh` | `run-viewer-run-list`, `run-viewer-attention`, `run-viewer-flow-halted`, `run-viewer-config-risk`, `run-viewer-config-assert` (dark) |
| `approval` | `shots-approval.sh` | `run-viewer-agent-halted`, `run-viewer-agent-verified`, `run-viewer-timeline`, `run-viewer-share` (dark) |
| `deadline` | `shots-deadline.sh` | `run-viewer-deadline-attention`, `run-viewer-gate-expired` (dark and light) |
| `identity` | `shots-identity.sh` | `run-viewer-identity-attention`, `run-viewer-identity-run` (dark and light) |
| `flows` | `shots-flows.sh` | `flow-gate-waiting`, `flow-gate-approved`, `flow-diff-strengthened`, `flow-diff-weakened`, `run-viewer-config-problem` (dark) |
| `editor` | `shots-edit.sh` | `flow-edit-weakened`, `flow-edit-save` (light, 1280 x 900) |
| `needs-attention` | `browser-shots/browser-shots.sh` | `run-viewer-needs-attention` (dark and light, 2x) |
| `live` | `live.sh` | `run-viewer-live-dark.gif` |

`browser-shots/browser-shots.sh` also makes the approve page and the Slack relay images
(`ONLY="approve relay"`), which `scripts/shots.sh` leaves alone: the approve page changes with its own pull
request. `cli-*.png` and `refund-*.png` are terminal images, made by `cli.sh` and `record.sh`.

## When

Regenerate before each release, as part of the final docs pass: the viewer, the editor and the themes
change between releases, and a picture that was right a month ago is not a promise. Run
`scripts/shots.sh`, look at each image, and commit the ones that changed with the release's docs.

## How a picture is taken

One program, `browser-shots/capture.mjs`, takes every viewer and editor picture (the share card is
the file the page's own button saves, `share.mjs`; the GIF's frames are `live-capture.mjs`). The
page is shown in the named color scheme (`prefers-color-scheme`) with `prefers-reduced-motion:
reduce`, so the graph's flowing dashes and the live pulse stand still; the window is 1200 pixels
wide (900 for the live GIF, 1280 for the editor) at 1x, scrollbars are hidden, and the program waits for
the network and the fonts to settle before it photographs. The height of each picture is the one
its script names, so the guide's image sizes do not move.

The data is fixed where it can be: `bide sample` writes the same runs on every call, and the
examples' flows and configs are files in the repository. The scripts name their ports and build
the binaries they use from the checkout.

## What is not frozen

- **Wall-clock times.** Pages that run the examples live (the approval and flow runs, the deadline and
  identity demos) show the time of the run, and ages ("14d ago") move with the date you run it. A new
  picture differs from the last one in those digits.
- **Fonts and Chrome.** The pages use the system font stacks, so the machine's fonts decide the
  letterforms; the images in the repository were taken on macOS (Apple's system fonts) with Chrome
  for Testing 152. A different Chrome or system moves text by a pixel.
- **The Chrome the scripts find.** `CHROME_BIN` first, then a Chrome for Testing that puppeteer
  installed, then the usual paths. Set `CHROME_BIN` to pin it.

Install: the first run installs puppeteer-core from `browser-shots/package-lock.json` with
`npm ci --ignore-scripts`; nothing is downloaded but that package.
