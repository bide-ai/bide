# Flow editor: `bide edit`

`bide edit flow.yaml` opens a local page for one flow config: its declared graph, an outline of its
nodes and wiring, each node's approval gate and safety, the assertions and their verdicts, the
problems `bide check` finds, and the source. The design is in
[docs/design/visual-editor.md](../design/visual-editor.md); this page says what the command does
today.

**You can edit a node's approval gate, safety and block with forms, or edit the file's source, see
what `bide check` says about it as you type, see whether the edit weakens the flow, and save.** The
forms mean a policy owner need not write YAML, and an assertion is built from a template and shows its
verdict before it is added.

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

## What it does and does not do

The first phase of the editor (the accountability editor) is complete as designed: the
read-only views, the baseline and the weakened banner, the source pane with a diff and an atomic save,
the forms for a node's safety, block and approval gate, the assertions panel, and the accessibility
work below. The second phase (structure) is being added in steps; so far you can add, remove and
rename a node, write the entry (see [Nodes](#nodes-add-remove-rename-and-the-entry)), reorder nodes and
wiring elements, and set the flow's name and types (see [Order, wiring order and the flow's
settings](#order-wiring-order-and-the-flows-settings)). It does **not**
yet: wire edges, switches and joins (edit the source for those); know your `plan.Registry` (it checks structure only, so no block palette,
type-compatible connections or digest); compare with a journaled run; or let you place nodes on the canvas
(the layout is derived from the topology, on purpose). A config with an anchor or alias anywhere,
a repeated key, a key that is not a string, NaN or infinity, or mixed line ends is refused by the editor
with the reason (edit it in a text editor); `plan` itself accepts an anchor or alias, and refuses the others too. The design note
([visual-editor.md](../design/visual-editor.md)) has the later phases.

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
- **The inspector**: the selected node's crash behavior, its approval gate and its problems, and the
  forms below (see [The inspector's forms](#the-inspectors-forms)). Select a node by clicking it in
  the flow, or with the keyboard in the outline or the flow (Tab to it, Enter).
- **The assertions**, each with its verdict and, for a violated one, the counterexample path. They
  are checked by `bide check` and in CI, not at run time: `plan.Load` ignores `assert` lines.
- **The problems** `bide check` lists, and **the source** as the file is on disk.

## The inspector's forms

Select a node and the inspector offers three forms. Each change is an **operation** the server
applies to the file's text as a splice, so the diff of an edit is the edit: the file's comments,
blank lines, key order and the flow or block style of the lines around it stay as they were. The
result is verified before it is returned (the text must parse to the old data with the operation
applied and nothing else, every other line unchanged, every comment still with its entry); an
operation the engine cannot splice safely (a file with anchors or aliases on that node, more than
one document, CRLF line ends, a byte-order mark) is refused with the reason and the text is
untouched, and you edit that part in the source.

- **Safety**: `readonly`, `idempotent` or `side_effect`, or left to the block. The config may only
  lower the safety a block declares in Go, never raise it; the editor cannot see the block's, so a
  value above it fails when a program loads the flow, and the form says so.
- **Block**: the name of a registered block. You choose one (the names used in the file are
  offered); a block's Go code is not written here.
- **Approval gate**: decisions needed, the approvers in policy order, **separation of duties** (never
  count the person who started the run) and a **deadline** (`timeout`, a Go duration such as `48h`:
  past it the gate expires and the node never runs). The form edits a draft. Every change to the
  draft asks the server what `bide check` would say of the node with that gate and shows the answer
  under the form, in the check's words (`approval.need 9 must be between 1 and 4`, or an `assert` line
  the draft would break), so the form rejects exactly what the check rejects and there is no second
  validator in the page. **Apply gate** writes the draft into the text; **Remove gate** removes it.
  The page says that approvers' keys are supplied when a program loads the flow and that it cannot
  check that they are enrolled.

A form change is an undoable step (the history keeps the last 100 steps; each is a whole copy of the text): **Undo** and **Redo** in the header restore the exact earlier
text, and a burst of typing in the source is one step. After a change the focus returns to the
control you changed. A changed gate, safety or block is compared with the baseline like any other
edit, so the weakened banner appears when the change weakens the flow, with the same words as in CI.

## Nodes: add, remove, rename and the entry

These are operations like the forms above, and are checked the same way: the server applies the
change as a splice, then checks that the result decodes to exactly what the operation should produce
(computed from the operation and the old data alone, not from the splices), and returns nothing it has
not verified. The node controls are in the outline (**Add a node**) and the inspector (**Node**).

- **Add a node**: a name and a block. The node goes at the end of the node list in the style of the
  last one, and is wired to nothing yet. The name must be new, not a join's name, and have no padding
  or control character, and no invisible, direction-changing or non-plain space character (so a name cannot read as another in a diff; an emoji sequence that needs a zero-width joiner is refused too).
- **Names must print.** plan refuses an identifier with a character that does not print or that draws as
  nothing (see [Load-time validation](plan-flows.md#load-time-validation)): the editor refuses it in a new name,
  by the same rule, so a file it writes loads. That includes the zero-width joiner and non-joiner, every
  variation selector (so an emoji written with one, such as a red heart as U+2764 U+FE0F, or a keycap, is not
  a valid name; the bare symbol is) and a left-to-right or right-to-left mark. A file that already has such a
  name (typed in the source) does not load, and the page says which field and writes the character out. The
  page also writes any such character as `\u{code}` wherever it draws a name, a title or a message, with a
  note to rename it, because a name can reach it by another road (a block name a program registers in Go,
  in typed mode). The rename field keeps the raw text.
- **Rename** rewrites the name everywhere the file uses it: the node, the `entry`, edges, a switch and
  its arms and `else`, a join's inputs, and `assert` lines. An assertion is rewritten through the
  assertion grammar and never by text search, so an assertion line that mentions the node but is not
  one of the six forms (or a name the grammar cannot hold, such as one with a double quote) makes the
  rename refuse with the line, and nothing changes. **A rename of a gated node reads as weakened.** The
  gate comparison follows names, so it sees the old node removed and a new, ungated one added; the
  banner says so, and so does `bide check -diff`. This is on purpose: the check over-reports rather
  than trust the editor to say which names are the same node.
- **Remove node** first asks the server what else goes with the node and shows that list: the edges
  that name it, the arms and `else` that route to it (a switch left with no arm goes whole), and the
  comments attached to it. Nothing is removed until you confirm that list, and the server refuses a
  removal whose list is not the one it computes, so what is removed is what you were shown. A node
  that is an input of a join is not removed (change the join first). `assert` lines that name the
  node are listed and **left in place**: removing an assertion would weaken the policy, and the
  check reports the line as naming a node that is gone. The list covers what names the node, not every effect: removing a node can leave an assertion that does not name it violated or vacuous (`b reachable` after the only path to `b` is gone), and that is caught by the weakened banner and the save confirmation, not by the dialog. **Undo** brings a removal back.
- **The entry node.** A config with no `entry` key starts at its first node, and the topology digest
  commits to the entry. The editor never changes the entry without you: adding a node never does,
  but removing the entry node, or the first node of a config with no `entry`, is refused, and the
  refusal points at **Make this the entry node**, which writes `entry:` into the file. Clearing the
  entry is allowed only when the first node is the entry already.

## Order, wiring order and the flow's settings

Order is data: the topology digest commits to the order of the nodes, of the edges and arms, and the
flow's name. These controls change it and nothing else.

- **Move up / Move down** in the inspector exchange the selected node with its neighbour, and the
  inspector says the node's position ("Position 2 of 4"). The two nodes keep their own text exactly: the
  comments directly above each, the trailing comment on it, its style and its keys. A blank line or a
  comment that is the file's, between them, stays where it was. A config with no `entry` key starts at
  its first node, so a move into or out of the first place is refused until the entry is written
  (**Make this the entry node**).
- **Wiring** is a list under the outline, one Tab stop with the arrow keys between its elements and
  Enter to select, and one **Move up** / **Move down** pair for the selected element, however long the
  list is. A comment above an element moves with it.
- **Flow name and types** (a disclosure under the outline) set the flow's `flow` label and the optional
  `in` and `out` type names, which a program checks against its Go types when it loads the flow; an
  empty `in` or `out` removes the key. The flow's name is in the digest, so changing it changes the
  digest.

Each is an exchange of two neighbouring list items, or a change of one value, applied as a splice and
verified: the result must be the old text with those two items exchanged and nothing else (the same
lines in another order, every line outside the pair where it was, each item with the comments it had
above it), checked independently of the code that made the splice.

## The assertions panel

Below the flow, the panel lists the config's `assert` lines, each with its verdict (`holds`,
`violated` with the counterexample path, `undecided`, `invalid`), and a form to add one. **An
assertion is built from a form, not typed.** The six forms of the grammar are the templates:

| Template (its name starts the label) | Writes |
|---|---|
| Approval: every path to a node passes a gate of at least K decisions | `every path to <node> passes approval >= K [of N] [excluding the requester]` |
| Passes: every path to a node passes another node | `every path to <node> passes <node>` |
| Terminal: every path to a terminal passes a node | `every path to a terminal passes <node>` |
| No side effect: nothing with a side effect comes after a node | `no side effect after <node>` |
| Reachable / Unreachable: a node can, or cannot, be reached | `<node> reachable`, `<node> unreachable` |

The page sends the form's parameters and the server writes the line, in the grammar `plan` reads,
with a node name that has a space in double quotes; a name the grammar cannot hold (one with a double
quote or a tab, or two spaces in a row) is refused, and only declared nodes can be chosen. **While you
fill the form, each change asks the server what the check says of the line and shows the verdict**,
the counterexample path of a violation included, before anything is added: the same call as an
edit, not kept. A line the config already has is refused. **Add assertion** appends a line (creating
the `assert` list if there is none), **Edit** (for a line that is one of the six forms) replaces it in
place, **Remove** removes it (and the list, with its last line). Each is a splice, so a comment above
a removed line goes with it and a trailing comment on a replaced one stays.

**Assertions are checked by `bide check` and in CI, not at run time:** `plan.Load` ignores `assert`
lines and they are not in the digest, so adding one adds a check CI can fail on and not a guard on
the run. The panel says so beside the list. Removing or loosening one is weakening, and the banner
says so like any other weakening; an `assert` line that stops holding because of another edit is too
(see [The baseline and the weakened banner](#the-baseline-and-the-weakened-banner)).

## The baseline and the weakened banner

Every version of the file the page shows is compared with a **baseline**, the committed policy: the
file at git `HEAD`, or at the revision `-base` names (a branch, tag or commit; `bide edit -base
main flow.yaml`), or, when the file is not in git, the file as it was when `bide edit` opened it.
The baseline is fixed when `bide edit` starts, so weakening in two edits is still weakening
against the commit. The editor reads git only for this, with the same `git show rev:./file` that
`bide check -diff` makes.

![The editor in light mode after the approval gate form on the charge node was used to turn off separation of duties: a red APPROVAL GATE WEAKENED banner names the change from 2 of 3 excluding the requester to 2 of 3, a green ALL ASSERTIONS HOLD banner sits below it, and the flow, outline and inspector with its forms are visible](img/flow-edit-weakened.png)

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

![The save dialog over the same page: a red THIS WEAKENS THE POLICY box repeats the change and holds an unchecked I confirm that I am weakening this policy box, with the unified diff of the file below it showing the removed exclude_principal line, and a Save button that stays disabled until the box is checked](img/flow-edit-save.png)

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

What a rename-over-the-file save does not keep: extended attributes, ACLs and SELinux labels, and, on
Windows, it cannot replace a file another program holds open without sharing delete. Permission bits
are kept, and so are the owner and group where the user may set them. A save is refused, and nothing
is written, for a file that is read-only (the editor does not replace what its owner protected) and,
on Unix, for a file that has other names (hard links), which would be left with the old text. Saves
in one editor are serialized: of two over the same hash exactly one succeeds. The directory is opened
once at startup and every read and write goes through that handle, so a directory swapped for a link
later cannot redirect a save, and a file swapped in at the config's name is refused. A process killed
mid-save leaves a private `.bide-edit-*` file beside the config, which the next `bide edit` in that
directory removes when it is ours and over ten minutes old.

A file that is not a readable config (invalid YAML, a duplicate key, more than one YAML document)
shows `CONFIG DOES NOT PARSE` and the reason, and no graph.

## A policy owner's workflow

1. `bide edit flows/refund.yaml` in a checkout. The banner is empty: the file is the committed policy.
2. Select the node, change its gate (raise `need`, add an approver, turn on separation of duties, set a
   deadline) or an assertion. The verdict, the problems and the banner follow each change; undo is
   one button.
3. If the banner says `APPROVAL GATE WEAKENED`, the change loosens the policy against the commit.
   Undo it, or go on knowing that saving needs the confirmation and that CI will refuse the pull
   request until a policy owner approves it.
4. Save: read the diff (it is the edit and nothing else), confirm if asked, and commit the file with
   git. In CI, `bide check flows/refund.yaml -diff main -check -fail-on-weaken` is the control; the
   editor shows the same verdict earlier.

## Accessibility

The editor is built to be used without a pointer and without dragging.

- **No operation needs a pointer or dragging.** Every control is a native button, field, select or
  checkbox, reached by Tab; one test drives the path of selecting a node, setting its safety, editing
  its gate, adding an assertion, undoing and redoing, and saving with the confirmation a weakening
  needs, by keyboard alone (the other controls are not each driven by a test). The outline is one Tab
  stop and the arrow keys, Home and End move between its nodes; Enter or Space selects. The nodes in the
  drawing are buttons too (Tab to them, Enter). The wiring list works the same way, with one Move pair for the selected element, and
  the flow's settings are a closed disclosure, so neither costs a Tab stop per element or field. Forms are real forms: every field has a label, an error
  is tied to its field, and the draft of a gate or an assertion says what the check thinks in a live
  region, rewritten only when its words change. There is no drag in this phase and none will be
  required.
- **Changes are announced** through a live region (the selection, `Saved`, an operation done or
  refused, the verdict, and an alert for the weakened banner), and the focus returns to the control
  you changed. The save dialog is a modal dialog: Tab stays in it, Escape closes it, and the focus
  goes back to where it was.
- **State is not carried by color alone.** Verdicts, assertion states, gate badges and the weakened
  banner each have a glyph and words, the selected node in the outline a heavier edge, a bold name and a
  marker (a test checks the computed styles); the diff marks lines with `+` and `-`. The two themes
  (light and dark) follow the system and can be switched; the drawing's flowing dash stops under
  `prefers-reduced-motion`.
- **Tested.** `go test ./flowedit` drives the page in real Chrome (the `Editor (Chrome)` job):
  the whole path by keyboard alone (the script never uses the pointer and bounds the Tab presses it
  needs), and axe-core over the page in light and dark in five states (as opened, a node selected
  with its forms, a weakened banner, an assertion form with a violation, the save dialog), failing
  on any violation.

An automated check finds a fraction of the problems a person meets. **A manual screen-reader pass
has not been done:** the checks above are not a substitute for one, and the checklist for it is in
[docs/KNOWN-LIMITATIONS.md](../KNOWN-LIMITATIONS.md#the-flow-editor).

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
its approval gate: `need`, approvers, `exclude_principal`, `timeout`; add, replace or remove an
assertion from one of the six forms, `flowedit.Assertion`) to a config's text by splicing,
so comments, blank lines and key order stay and the diff of an edit is the edit; each result is
verified, and what cannot be spliced safely is a `*flowedit.Refusal` with the text untouched. It is
what the forms call, through `POST /api/apply`. `flowedit.UnifiedDiff` is the diff the dialog shows, and
`(*flowedit.Server).Save` is the save, with the checks above.
