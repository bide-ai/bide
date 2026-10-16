# Flow editor: `bide edit`

`bide edit flow.yaml` opens a local page for one flow config: its declared graph, an outline of its
nodes and wiring, each node's approval gate and safety, the assertions and their verdicts, the
problems `bide check` finds, and the source. The design is in
[docs/design/visual-editor.md](../design/visual-editor.md); this page says what the command does
today.

**In this release the page shows the config and does not change it.** The next releases add editing
a node's approval gate, safety and block, a diff against the committed version with a banner when an
edit weakens the flow, and saving. Until then `bide edit` is a second way to read what `bide check`
and `bide view -config` show, in one page with an outline and an inspector.

The editor is an authoring tool and nothing else. It reads and (later) writes the file `plan.Load`
reads, adds no key to it, starts no run and holds no state of its own: the run is the run `plan`
already makes. A flow is checked by the code `bide check` runs, in Go; the page holds the text and
draws what the server computes.

```sh
bide edit flow.yaml
# bide edit: serving http://127.0.0.1:8585/#t=3f9a... (loopback only, read-only in this release; Ctrl-C to stop)
```

Open the whole address, including the part after `#`: it is a random session token the page sends
with every request, and it is not sent to the server as part of the address or in a `Referer`.

## What the page shows

- **The verdict**, in the words `bide view -config` uses (`ALL ASSERTIONS HOLD`, `1 POLICY
  VIOLATION`, `2 CONFIG ERRORS`, `CONFIG DOES NOT PARSE`), with a glyph beside the word: a state is
  never carried by color alone. Under it the page says **structural check only: blocks and types are
  checked when a program loads the flow**. From the command line a block's own gate and safety,
  declared in Go, are not visible, so a clean verdict here is not `Load` accepting the flow (see
  [Check a flow config](run-viewer.md#check-a-flow-config)).
- **The flow**: the same layered drawing `bide view -config` draws, with each node's crash behavior,
  its gate badge (`2/3`) and the problems marked on the node or edge they belong to. The layout is
  derived from the topology, not stored: the config has no place for coordinates, so the picture is
  the one a reviewer sees on `bide check -diff`.
- **The outline**: the nodes in the order the config declares them, then the wiring. Order is data
  (the topology digest commits to node, edge and arm order), so the outline is a list in config
  order, not a sorted one.
- **The inspector**: the selected node's block, the safety the config states, the crash behavior the
  check derives, its approval gate and its problems. Select a node by clicking it in the flow, or
  with the keyboard in the outline or the flow (Tab to it, Enter).
- **The assertions**, each with its verdict and, for a violated one, the counterexample path. They
  are checked by `bide check` and in CI, not at run time: `plan.Load` ignores `assert` lines.
- **The problems** `bide check` lists, and **the source** as the file is on disk.

## The baseline and the weakened banner

Every version of the file the page shows is compared with a **baseline**, the committed policy: the
file at git `HEAD`, or at the revision `-base` names (a branch, tag or commit; `bide edit -base
main flow.yaml`), or, when the file is not in git, the file as it was when `bide edit` opened it.
The baseline is fixed when `bide edit` starts, so weakening in two edits is still weakening
against the commit. The editor reads git only for this, with the same `git show rev:./file` that
`bide check -diff` makes.

The comparison is the classifier `bide check -diff -check -fail-on-weaken` runs, not a second one:
the editor shows what it returns. When the text is weaker than the baseline (a gate removed, its
need lowered, an approver added without raising the need or replaced, `exclude_principal` or a
deadline dropped or lengthened, an `assert` line removed, loosened or no longer holding, an effect
newly reachable without a gate, a block changed on an ungated effect node) the page shows a banner
at the top: a glyph, `APPROVAL GATE WEAKENED` (or `SAFETY CHECK WEAKENED` when no gate policy itself
changed), and each item (`charge: approval 2 of 3 -> 1 of 3`). **The banner has no close control**:
a banner that can be closed is a warning that can be missed. It stays until the text no longer
weakens, and it is announced to a screen reader as an alert. Below the verdict a line says what the
file is compared with and how many changes there are. If the text cannot be read as a config, the
page says it cannot say whether it is weaker, which is not the same as not weaker.

What the classifier does not see applies here as it does in CI: from the command line a gate a
block declares in Go is invisible on both sides, and a change of block on an ungated effect node
is reported as weakened for that reason (see [Compare two versions of a
flow](run-viewer.md#compare-two-versions-of-a-flow)).

A file that is not a readable config (invalid YAML, a duplicate key, more than one YAML document)
shows `CONFIG DOES NOT PARSE` and the reason, and no graph.

## Where it runs, and what it can touch

`bide edit` serves on a loopback address (`-addr`, default `127.0.0.1:8585`) and refuses any other
host, as `bide view` does. It reads only the file named on the command line: that file must exist,
be a regular file of at most 1 MiB and end in `.yaml`, `.yml` or `.json`; a symbolic link is
resolved once at startup. There is no endpoint that opens another file.

The page is served by the binary, with no CDN, no web font and no request to another host, under a
Content-Security-Policy that allows only same-origin script, style and requests and no inline
script or style. Every request the page makes is a `POST` of `application/json` from the page's own
origin carrying the session token, and the body is limited to 1 MiB; a request without them is
refused. Config text is data: node names, block names and messages are put in the page as text,
never as markup, and the graph (SVG the server escapes) is parsed as an XML document and adopted.

This defends against another web page or local process reaching the editor, and against a hostile
config attacking the person who opens it. It does not defend against someone who controls the
machine, who can edit the file directly.

`bide edit` is in the full `bide` binary and not in a `bide_audit_only` build, which holds no YAML
reader and writes no files.

## Flags

| Flag | Meaning |
|---|---|
| `-base` | Compare with the file at this git revision instead of at `HEAD`; the file must be in it, or `bide edit` exits 1. |
| `-addr` | Address to serve on: a loopback IP (`127.0.0.1` or `::1`) or `localhost`, with a port. Default `127.0.0.1:8585`. |
| `-color` | `auto`, `always` or `never`, for the line it prints. |

Exit status: 0 after a clean stop, 1 if the file cannot be read or the server fails, 2 for a usage
error (including a non-loopback address).

## Embedding

The command is a thin wrapper over package `github.com/bide-ai/bide/flowedit`: `flowedit.New(path)`
returns a `*Server` whose `Handler()` is an `http.Handler` that does not depend on a listener or on
the address, so another host can serve the same page. `flowedit.BuildView` is the page's data for
a text: the same answers `bide check` gives, as a value. `flowedit.Compare` is the weakening verdict
for a text against a baseline, and `flowedit.WithBase` is the `-base` option of `flowedit.New`.
`flowedit.ApplyOperation` applies one editor operation (set a node's safety or block, set or clear
its approval gate: `need`, approvers, `exclude_principal`, `timeout`) to a config's text by splicing,
so comments, blank lines and key order stay and the diff of an edit is the edit; each result is
verified, and what cannot be spliced safely is a `*flowedit.Refusal` with the text untouched. The
page does not offer these operations yet.
