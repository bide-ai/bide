# Flow editor: `bide edit`

`bide edit flow.yaml` opens a local page for one flow config: its declared graph, an outline of its
nodes and wiring, each node's approval gate and safety, the assertions and their verdicts, the
problems `bide check` finds, and the source. The design is in
[docs/design/visual-editor.md](../design/visual-editor.md); this page says what the command does
today.

**In this release you edit the file's source in the page, see what `bide check` says about it as you
type, see whether the edit weakens the flow, and save.** The next release adds forms for a node's
approval gate, safety and block, so a policy owner need not write YAML.

The editor is an authoring tool and nothing else. It reads and writes the file `plan.Load`
reads, adds no key to it, starts no run and holds no state of its own: the run is the run `plan`
already makes. A flow is checked by the code `bide check` runs, in Go; the page holds the text and
draws what the server computes.

```sh
bide edit flow.yaml
# bide edit: serving http://127.0.0.1:8585/#t=3f9a... (loopback only; Ctrl-C to stop)
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
deadline dropped or lengthened, an `assert` line removed, loosened or no longer holding, a node
newly reachable without a gate (whatever its safety label; a lowered `safety` is itself reported), a block changed on an ungated effect node) the page shows a banner
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

## Editing the source and saving

The **Source** pane is a text area with the file's text. Typing in it asks the server for the check
of the new text after a short pause, and every pane (the verdict, the flow, the outline, the
assertions, the problems and the weakened banner) updates: the page validates nothing itself, so
there is one validator, the one `bide check` runs, and a text the page accepts is a text CI accepts.
Nothing is written until you save, and there is no autosave: a policy file should not change on a
keystroke. The header says `saved` or `unsaved changes`, **Revert** puts back the text as it was last
read or saved, and the page warns before it closes with unsaved changes.

**Save** opens a dialog that shows what will be written:

- the plain unified diff of the bytes against the file on disk, so formatting churn and removed
  lines are seen before they are written;
- every **comment the diff removes**, listed, because a dropped comment is easy to miss in a long
  diff;
- how many **errors** the text has: a file with errors can be saved (a half-built flow must be
  holdable) and the dialog says it will fail `bide check` and `plan.Load` until they are fixed;
- if the text is **weaker than the baseline**, `THIS WEAKENS THE POLICY` with each item, and a
  checkbox: Save stays disabled until you confirm. The confirmation lists exactly the items the
  server computed for this text and is checked by the server, not only by the page: a save of a
  weakened text without it is refused (HTTP 422) and writes nothing, and a confirmation made for an
  older text does not cover a newer one. A text that could not be compared needs the same
  confirmation. The confirmation is not written into the file, which has no place for one and must
  not grow one: the control that stops a weakened policy merging is the pull request, where `bide
  check -diff -check -fail-on-weaken` fails and a policy owner has to approve.

The write is **atomic**: the text goes to a temporary file in the same directory, which replaces the
file by a rename, keeping its permission bits, so a reader sees the old file or the new one and a
failure (a full disk, a permission) leaves the old file and no temporary file. The save carries the
hash of the file as the page read it, and if the file on disk is different the answer is a conflict
(HTTP 409): nothing is written, and the dialog shows the file on disk against your text and offers
to reload (which replaces your edits) or to keep editing. It never overwrites. This is a
compare-and-swap for a person editing in two places, not a lock: another process that writes
between the check and the rename is overwritten.

The path is fixed at startup and checked again when writing: a target that has become a symbolic
link, or a directory, or whose directory is no longer the one resolved at startup, is refused. The
editor does not run `git add`, `commit` or `push`; you commit the file.

What a rename-over-the-file save does not keep: hard links to the file (the new file is a new
inode, so another name for the old one is left behind), extended attributes and ACLs, and, on
Windows, it cannot replace a file another program holds open without sharing delete. Permission
bits are kept.

A file that is not a readable config (invalid YAML, a duplicate key, more than one YAML document)
shows `CONFIG DOES NOT PARSE` and the reason, and no graph.

## Where it runs, and what it can touch

`bide edit` serves on a loopback address (`-addr`, default `127.0.0.1:8585`) and refuses any other
host, as `bide view` does. It reads and writes only the file named on the command line: that file must exist,
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
page does not offer these operations yet. `flowedit.UnifiedDiff` is the diff the dialog shows, and
`(*flowedit.Server).Save` is the save, with the checks above.
