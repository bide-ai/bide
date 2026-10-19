# Design: themes for bide's visual pages

Status: proposed. Nothing in it is built. The decisions it needs from the maintainer are at the end;
no implementation starts until they are made. Written against `main` at c0224a63. The target is
v0.22.0.

The visual pages are the run viewer (`bide view`, `internal/runview/assets`), the flow editor
(`bide edit`, `flowedit/page`), the diff and config pages (the viewer's, also served by
`bide check -diff` and `bide view -config`), and the approve page (`approvals/page`). The canvas (the
server-drawn flow graph) is shared by the viewer, the diff and the editor.

## Summary

- **Two layers of tokens.** Semantic tokens (ok, info, warning, alert, gated, side effect,
  idempotent, idle) have a meaning bide fixes; a theme supplies their values and is refused if the
  values fail the checks. Surface tokens (backgrounds, text, borders, accent, fonts, density, radius,
  syntax colours) a theme sets freely, inside ranges.
- **A theme is a small JSON file of typed values**, parsed by a stdlib-only Go package, checked, and
  turned into one generated stylesheet of custom properties. There is no way to write a selector, a
  property, a URL or a function.
- **Validation is numeric and stated:** WCAG 2.x contrast (4.5:1 text, 3:1 non-text), CIEDE2000
  distance between the status colours, and an advisory colour-blindness check. Shipped themes are
  checked in CI; a custom theme is refused at load with each failing check named.
- **State is never colour alone.** Today a few places are (list below); they are fixed before themes
  ship, because a theme with close hues makes them worse.
- **forced-colors and prefers-contrast:** the system wins. No page handles either today.
- **The approve page takes shipped themes only, inside its signed bundle**, chosen by the system
  settings, with no input from the host. A host-supplied name or logo is deferred.
- **No renaming of existing CSS variables.** The theme layer sets the names the pages already use, so
  the editor's CSS keeps working and the phase 2 work does not have to rebase over a rename.

## Goals and non-goals

Goals:

1. One place where a page's colours, fonts, spacing and radii come from, instead of the viewer's dark
   palette written twice in `style.css` and a separate, hand-written set of 18 hex colours in the
   approve page's `app.css`.
2. A high-contrast theme and a system-driven one, so the pages work for readers who need them, and
   `forced-colors` and `prefers-contrast` are honoured.
3. A host can fit the viewer and the editor to its own look (a bank's internal portal, a Go program
   that embeds `bide edit`) without writing CSS, and cannot break the pages' accessibility or meaning
   by doing so.
4. A theme cannot make a state unreadable, swap its meaning invisibly, hide a control, or restyle the
   approval UI.
5. No new dependency, no new request off the server, no build step, within the editor's 150 KB
   budget.

Non-goals:

- **Custom CSS, layouts, logos, fonts loaded from a file or URL, animations, per-flow or per-run
  themes, a theme in a flow config or a journal.** A flow config is read strictly and carries no
  presentation; a run's data cannot choose how it is shown.
- **Branding as a feature.** The editor's design note keeps the substrate, not the editor, the
  headline ("the editor becomes the pitch"). Themes are documented in a guide, not on the README.
  A theme changes how the pages look and never what they say.
- **A theme editor, gallery or marketplace.** `bide theme check` (below) is the only tooling.
- **Theming the Mermaid export or the tab icon's palette.** They are fixed (see the audit); a
  Mermaid diagram is text pasted elsewhere.
- **Closing the screen-reader gap.** Themes do not change the accessible tree (a test pins that), and
  the manual screen-reader pass in KNOWN-LIMITATIONS stays owed.

## What exists today

| Piece | Where | State |
|---|---|---|
| Viewer tokens, light and dark | `internal/runview/assets/style.css` | One light set and the dark set written twice (once under `prefers-color-scheme`, once under `data-theme="dark"`), 20 colour tokens and 6 syntax colours, space, type, radius, fonts |
| Theme choice | `theme.js` (before paint), `view.js` toggle (key `t`), `localStorage["bide-theme"]` | Two values, `light` and `dark`; unset follows the system |
| Editor | `flowedit/page/index.html` loads the viewer's `style.css`, `graph.css`, `extras.css`, then `editor.css`; `editor.js` has its own `theme()` toggle | Shares the viewer's tokens |
| Approve page | `approvals/page/app.css` | No tokens: 18 distinct hard-coded hex values; dark only by `prefers-color-scheme`; no toggle |
| Canvas | `svg.go` draws classes only; `graph.css`, `diff.css` colour them | No literal colour in the SVG; all through the tokens |
| Fixed colours | `head.go` (`faviconColors`, the tab icon), `graph.go` (the Mermaid `classDef`) | Literal hex, outside any theme |
| Delivery | `Content-Security-Policy: default-src 'self'; style-src 'self'` (viewer), `style-src 'self'` and `script-src 'self'` (editor, approve page) | No inline style is allowed, so a theme must arrive as a stylesheet from the same origin |
| Approve bundle | `approvals.PageFiles`: `index.html`, `page.js`, `app.css`; `index.html` carries `sha384` SRI for the other two; `approve-page.SHA256SUMS` is signed in the release workflow and rebuilt and compared on another OS by the repro job | Deterministic by construction; any byte change changes the hashes |
| Live mode | `live.js` swaps page content and keeps the live stylesheet | A theme stylesheet must be kept across swaps too |

Facts that shape the design:

- The viewer's and the editor's CSS draw colour only through `var(--token)`, and `svg.go` writes
  classes, not colours. So the token layer can be swapped without touching a template or the SVG;
  only `approvals/page/app.css` has literal colours.
- The root module requires nothing and reads JSON; YAML lives in the `cmd/bide` module. The theme
  parser therefore reads JSON and lives in the root module, where `internal/runview`, `flowedit`
  (a nested module, which imports `internal/runview`) and `approvals` can all use it.
- `style-src 'self'` stops inline style but not custom properties set from a same-origin stylesheet.
  The generated `theme.css` is such a stylesheet, so no CSP change is needed.

## The tokens

### Meaning-bearing tokens (semantic layer)

Each has a foreground value (`fg`: text, stroke, glyph on the page surface) and a tint (`bg`: a fill
behind text of the `fg` colour). Today's CSS names are kept; the theme file uses the friendly name on
the left.

| Token (theme key) | CSS names today | Meaning, as the pages use it now |
|---|---|---|
| `ok` | `--good`, `--good-bg` | Completed, resolved, approved, verified, audited identity, read-only risk, added in a diff, a passing assertion |
| `info` (new) | none: the pages use `--accent` | Waiting for a person, the path taken, the last node, live, a pending gate. Defaults to `accent` |
| `warning` | `--warn`, `--warn-bg` | Started or in progress, attempted, provisional identity, changed in a diff, stale, a warning-level problem, a deadline running |
| `alert` | `--bad`, `--bad-bg` | Halted, aborted, held, denied, refused, a problem, a counterexample, removed in a diff, the weakened banner |
| `gated` | `--gate`, `--gate-bg` | An approval gate, the gate badge, an approval record in the timeline |
| `effect` | `--side`, `--side-bg` | A declared side effect |
| `idem` | `--idem`, `--idem-bg` | An idempotent step |
| `idle` | `--idle` (no tint) | Not reached, unknown risk, an edge not taken, a pending approver |
| `on` (new) | none: the pages use `--card` | A glyph or text drawn on a solid `fg` disc (the node discs, the edge mark) |

Named meanings the task list asked for are **aliases, not extra colours**, defined by bide and not by
the theme: path taken and the last node are `info`; refused, the weakened banner, a counterexample
and a halted node are `alert`; a strengthened or comparison-error banner is `warning`. Page authors
get stable names (`--m-path-taken`, `--m-refused`, `--m-weakened`) that point at the colour token.
Separate colours would let a theme draw a refused call differently from every other alert, and would
multiply the pairs the validator has to prove distinct (decision 3).

The glyph, the word and the shape that go with each meaning are bide's, not the theme's. A theme
cannot change an icon, a label, a dash pattern or a border style.

### Surface tokens (theme sets freely, inside ranges)

| Token (theme key) | CSS names today | Used by |
|---|---|---|
| `bg` | `--bg` | The page; table row hover |
| `surface` | `--card` | Cards, the graph wrapper, tables, dialogs, inputs; the approve page's `pre` |
| `surface2` | `--card-2` | Pills, `.ro`, `kbd`, JSON blocks, the diff block, idle node discs |
| `text` | `--fg` | Body text, node labels |
| `muted` | `--muted` | Secondary text, node kind, edge labels, table heads |
| `border` | `--line` | Decorative borders of cards, tables, rows |
| `border_strong` (new) | none: controls use `--line` | The edge of every input, select, textarea, checkbox and button (3:1, see the audit: today 1.4:1) |
| `accent` | `--accent`, `--accent-bg` | Links, hover edges, the selected outline item, the primary button, the current row |
| `on_accent` (new) | none: the primary button uses `--bg` | Text on the primary button |
| `focus` (new) | none: focus uses `--accent` | The focus ring. Defaults to `accent` |
| `syntax` (6 values) | `--j-key`, `--j-str`, `--j-num`, `--j-bool`, `--j-null`, `--j-punct` | JSON highlighting. Today these reuse status colours (a green string, an amber number), which says "ok" about a string; they become their own tokens |
| `canvas_bg`, `node_fill` | `--card` (twice) | The graph's ground; the fill of a node with no state. Default `surface` |
| `shadow` | `--shadow` | Enum `none`, `soft`, `strong`; the theme cannot write a shadow value |
| `scrim` | a literal in `editor.css`, `extras.css` | The alpha (0.40 to 0.80) of the black behind a dialog or the help overlay |
| `font_ui`, `font_mono` | `--font`, `--mono` | Enum of generic families (below) |
| `type_scale` | `--t-xs` ... `--t-hero` | A multiplier, 0.90 to 1.25 |
| `density` | `--s1` ... `--s7` | `compact` (0.85), `normal`, `roomy` (1.2) |
| `radius` | `--r-sm`, `--r-md`, `--r-lg` | Three integers, 0 to 16 px, `sm <= md <= lg` |

The canvas has no colour per node kind. Nodes are not coloured by kind today (agent, tool, gate are
told apart by icon and label); the colour on a node is its run state or its risk, which are semantic.
A per-kind colour would collide with them (a purple "gate kind" next to the purple gated badge), so the
note adds only `canvas_bg` and `node_fill` and no more (decision 5).

Fonts are an enum, not a family name: `system-sans` (the current stack), `system-serif`,
`system-mono`, `humanist`, `rounded`. A named family from the machine would let a theme pick a face
that renders `0` and `O`, or `1` and `l`, alike, on a page that shows digests and ids. Web fonts are
excluded by the CSP and the "no request off this server" rule.

## The theme file

JSON, strict (unknown keys are errors, duplicate keys are errors, at most 16 KiB), parsed by a new
root-module package (working name `theme`; stdlib only; public, so a Go host can build and check a
theme; 0.x API). A theme names a shipped base and overrides from it, so a custom theme can be five
lines:

```json
{
  "bide_theme": 1,
  "name": "acme-night",
  "base": "dark",
  "surface": {
    "bg": "#0e1116", "surface": "#161b22", "surface2": "#1f252d",
    "text": "#e6edf3", "muted": "#9da7b3", "border": "#30363d", "border_strong": "#7d8590",
    "accent": "#79b8ff", "on_accent": "#0e1116"
  },
  "semantic": {
    "ok":      { "fg": "#56d364", "bg": "#12261a" },
    "alert":   { "fg": "#ff938a", "bg": "#3a1d1b" }
  },
  "font_ui": "system-sans",
  "density": "normal",
  "radius": { "sm": 4, "md": 8, "lg": 12 }
}
```

Rules of the format:

- `base` is one of the shipped themes (`light`, `dark`, `high-contrast`). Every key left out is taken
  from the base. **The checks run on the resolved theme**, so inheritance cannot dodge them.
- `name` matches `^[a-z][a-z0-9-]{1,31}$` and is not a shipped name.
- A colour is `#rgb` or `#rrggbb`, nothing else: no names, no `rgb()`, no `var()`, no `calc()`, no
  `color-mix()`, no system colours, no alpha (alpha is only the `scrim` number). It is normalised to
  `#rrggbb` lower case on parse.
- A length is an integer in the key's range. An enum is one of its listed words. A number is in its
  range. Anything else is an error that names the key.
- `color_scheme` (`light` or `dark`) comes from the base unless set; it is what `color-scheme` is, so
  the browser's own controls (scrollbars, date fields) match.

### What becomes CSS

`theme.CSS` writes one block per theme, from the parsed, normalised values only:

```
:root[data-theme="acme-night"] { --bg:#0e1116; --card:#161b22; ... --r-sm:4px; color-scheme:dark; }
```

The only strings a user supplies that reach the output are the name (a fixed charset) and values that
parsed to a canonical form. The pages load `/assets/theme.css`, generated at startup, instead of the
token blocks in `style.css`. Light is the `:root` default. Dark applies under `prefers-color-scheme:
dark` when no `data-theme` is set, as now.

### Checks

All checks run on the resolved theme; each failure names the token, the measured value and the
threshold. Contrast is the WCAG 2.x ratio of relative luminance, computed unrounded (a 4.49 does not
pass as 4.5). WCAG 2.x is chosen over APCA because it is the standard an auditor and a bank's
accessibility policy name.

| # | Check | Pairs | Threshold |
|---|---|---|---|
| C1 | Text contrast | `text` and `muted` on `bg`, `surface`, `surface2`; every semantic `fg` on `bg`, `surface` and its own `bg` tint; `accent` on `bg`, `surface`, `accent` tint; `on_accent` on `accent`; `on` on every semantic `fg`; each `syntax` colour on `surface2` and `surface`; `text` on `node_fill` | 4.5:1 (4.5 even for large text: one rule) |
| C2 | Non-text contrast | `focus` on `bg` and `surface`; `border_strong` on `surface` and `bg`; `idle` on `bg` and `surface`; every semantic `fg` on `bg` and `surface` (strokes and icons; already implied by C1); `info` and `idle` (edges) on `canvas_bg` | 3:1 |
| C3 | Status colours are distinct | `ok`, `warning`, `alert`, `info` (`fg`), pairwise, CIEDE2000 in CIELAB (D65) | at least 20 |
| C4 | Domain colours are distinct | each of `gated`, `effect`, `idem` against every other semantic `fg` (and each other), CIEDE2000 | at least 10 |
| C5 | Distinct from the ground | each semantic `fg` against `bg`, `surface`: this is C2 (3:1); and each semantic `bg` tint against its `fg`: C1 | as C1, C2 |
| C6 | Colour-blind separation (advisory) | the C3 pairs after simulating protanopia, deuteranopia and tritanopia (Machado et al. 2009, severity 1.0) | at least 8; reported as a warning, not a refusal in v0.22.0 |
| C7 | Ranges | the format rules above, `radius` ordering, `type_scale`, `density`, `scrim` | as listed |

Why these numbers, from the current palette (measured with a throwaway script, not committed):

- Every current text pair passes 4.5:1 (lowest: `--side` on `--side-bg`, 5.4 in light, then `--bad` on
  `--bad-bg`, 5.5). C1 therefore does not force a visible change to light or dark.
- **C2 would fail the current themes in one place that matters:** `--line`, which every form control
  uses as its border, is 1.4:1 on a card in both themes. A control's edge needs 3:1 (WCAG 1.4.11).
  That is why `border_strong` exists: decorative borders stay quiet, control edges pass.
- **C3 passes** (current ok/warning/alert/info distances are 24.7 to 59.1 in light, 28.9 to 52.3 in
  dark in normal vision). The threshold 20 sits under the closest current pair (light warning versus
  alert, 24.7) and is what a theme must not drop below.
- **C4 fails today in one pair:** `idem` against `accent` is 4.5 in light and 2.5 in dark: the
  "idempotent" blue and the "waiting, path taken" blue are one colour, told apart only by glyph. The
  implementation retunes `idem` (a teal) so it passes; this is a visible change to existing pages.
- **C6 fails today**, by design of the current hues: ok, warning and alert are close in lightness
  (L* 39, 40 and 39 in light), so under deuteranopia warning versus alert is 1.8 and under
  protanopia ok versus alert is 8.5. That is why C6 is advisory: the state is carried by words and
  glyphs, and forcing a lightness ladder changes every verdict banner. The check reports it for every
  theme, shipped ones included, and the result is recorded in the guide (decision 6).

The validator is also a command, `bide theme check file.json`, printing the table above for the
theme with each measured value. A custom theme that fails C1 to C5 or C7 is refused: `bide view` and
`bide edit` exit before listening, with the report; a Go host gets an error from `theme.Parse` and
cannot construct an unchecked theme (the type has no exported fields and no other constructor).

## Where themes come from

| | Shipped | File | Go embedding |
|---|---|---|---|
| `bide view`, `bide check -diff`, `bide view -config` (the viewer's pages) | `light`, `dark`, `high-contrast`; the toggle cycles them, the system sets the start | `-theme file.json`, once; it joins the toggle and, with `-theme-default`, is the start | An option on the handler that takes a `*theme.Theme`, a list of them and a default, and a lock (no toggle) |
| `bide edit` | the same | `-theme file.json`, the same flag | The same option on the editor's handler (phase 3, a host that embeds the editor beside its Registry) |
| Approve page | `light`, `dark`, `high-contrast`, chosen by the system settings only | none | none (`approvals.PageFiles` is unchanged in shape) |

The file is read once, at startup, from a path the operator named on the command line: the same trust
as `-db`. Nothing reads a theme from a flow config, a run, a journal, a URL or the network. The page
only ever sees the generated `theme.css`.

Choosing among themes in the browser is `data-theme` on `<html>`, remembered in
`localStorage["bide-theme"]` as now. The remembered value is used only if it names a theme the page
loaded (`theme.js` checks the list the stylesheet declares); an unknown value falls back to the
system. With `prefers-contrast: more` and no remembered choice, the high-contrast theme applies.

For a host: the Go API takes a value that already passed the checks. A host can set which themes
exist, the default, and whether the user may switch. It cannot change what is below.

### What a theme cannot change

Any selector, property, pseudo-element, `content`, `display`, `visibility`, `opacity`, position or
z-index; an icon, a label or a word; a dash pattern, a border style or the shape of a node; the
layout; the CSP; the approve page; and which semantic token a state uses. It cannot read anything.

## The colour-only audit

"State is never carried by colour alone" holds on the viewer's nodes (every state has a glyph in the
disc and words), the verdict banner (word and glyph), pills and the diff text (`+`, `-`). Reading
`graph.css`, `extras.css`, `svg.go` and the approve page's CSS, these places rely on colour (or on a
hair of stroke width) for a state:

| # | Where | What differs only by colour | Fix |
|---|---|---|---|
| A1 | Edges `.edge.added` and `.edge.changed` (graph.css) | Both solid, 2.8 px; green versus amber. Nodes carry a glyph; edges carry nothing | Different dash or a marker: added solid, changed a long-dash with a `~` label tick, removed already dashed |
| A2 | Edges `.edge.taken` and `.edge.cex` | Same dash (10 6), same animation, 3 versus 3.5 px; blue versus red. A counterexample path and the path taken look alike without colour | A double stroke or a distinct dash for the counterexample, plus its label |
| A3 | Edges `.edge.problem` and `.edge.removed` | Dashes 6 4 and 6 5, both red. They appear on different pages, but one theme change should not make them one | Distinct dash and the `!` mark the problem edge already has; give `removed` the strike or ghost style |
| A4 | The node mark (`.nmark`, `.edge-mark`) | A problem and a warning both draw a `!` in a circle; colour tells them apart | A different glyph for the warning (a `?` or a triangle) |
| A5 | Node boxes `.node.problem` and `.node.warning` | Same solid box; fill and stroke colour, 2.8 versus 2.2 px | The mark of A4, and a dashed box for warning |
| A6 | Form control edges (`--line`) | A field's boundary is 1.4:1 against its card: for readers with low vision the field is not seen (contrast, not state) | `border_strong` (3:1) |
| A7 | Timeline `.ev.fresh` (live) | A row added by the last poll is a tint and an inset 1 px ring | A "new" text badge (the row's `seq` already changes colour only) |
| A8 | Run list `tr.cur` | The current row is an accent tint | A marker in the first cell |
| A9 | Stat numbers `.s-good`, `.s-bad`, `.s-warn` | The number's colour is a judgement ("this count is bad") the label does not state | A word or glyph beside the number, or no colour |
| A10 | Approver identity blocks `.id-audited`, `.id-final`, `.id-bad` (extras.css) | Three solid left borders in three colours. The badge text (AUDITED and so on) carries it, so this is redundant colour, not colour alone | None; recorded so the test does not flag it |
| A11 | Approve page `.ok` and `.bad` result text, the `.warn` left border | The result sentence has words; the colour is redundant. `.warn` has a border and words | None; recorded |
| A12 | Disabled buttons on the approve page | Grey fill only; the `disabled` attribute carries it to assistive technology, and the pointer shows `not-allowed` | A dashed edge |
| A13 | JSON syntax colours | Key, string, number, bool and null differ by colour (bool bold, null italic) | None: the colour is decoration and the text is the content |
| A14 | The tab icon (`head.go`) | Colour by verdict class, but it holds the verdict's glyph | None |

A1 to A5 and A7 to A9 are fixed in the colour-only PR, before themes ship. A6 is the `border_strong`
token. Items marked "none" are listed so the test (below) has an explicit allow-list and a new
colour-only use fails it. The edge items (A1 to A3) are read from the CSS, not from a rendering; the
Chrome test below is what confirms them.

## forced-colors and prefers-contrast

Today no page has a `forced-colors` or `prefers-contrast` rule. Two behaviours:

- **`prefers-contrast: more`**: the `high-contrast` shipped theme applies (unless the reader chose a
  theme). It meets 7:1 for text and 4.5:1 for status colours, 3:1 for control borders at 2 px, and its
  borders and strokes are thicker (a token-driven `border_width`, not a free key).
- **`forced-colors: active`**: the system wins. The theme file and the shipped themes are ignored
  (the browser overrides author colours anyway); `forced-color-adjust: none` is never used. The
  pages declare, under `@media (forced-colors: active)`, the system-colour mapping so what the
  browser would otherwise flatten still reads:

| Token | System colour | Because |
|---|---|---|
| `bg`, `surface`, `surface2`, `canvas_bg`, `node_fill`, every tint | `Canvas` | The ground |
| `text`, `muted`, `ok`, `warning`, `alert`, `gated`, `effect`, `idem` (`fg`) | `CanvasText` | The system offers one text colour; meaning cannot ride on it |
| `info`, `accent`, `focus` | `LinkText` for links and `Highlight` for the focus ring | Interactive things stay recognisable |
| `idle`, `border` | `GrayText` | Quiet |
| `border_strong` | `ButtonBorder` | Controls |
| `on`, `on_accent` | `Canvas`, `HighlightText` | Paired with their fill |

The consequence is that in forced-colors every status collapses to one colour. The **shape ladder**
then carries state, and is a requirement on the pages, not on themes: ok is a solid thin box with the
check glyph; info a double or thick solid box with the hourglass; warning a dashed box with the warning
glyph; alert a thick solid box with the halt glyph and an extra outer ring; gated a thick box with the lock
badge; idle a dotted box. Nodes already differ in dash and width for some of these; the ladder
is made complete in the colour-only PR so that A1 to A5 are fixed once, for greyscale and for forced
colors together. Whether Chrome and Firefox force SVG `fill` and `stroke` in forced-colors is to be
confirmed in the test (the CSS Color Adjust list includes them; this note does not assume it): the
ladder must hold whichever way it goes.

## The approve page

The hard part: the page's integrity is its pin. `index.html` carries SRI for `page.js` and `app.css`;
the organization checks `approve-page.SHA256SUMS` (signed by the release, rebuilt on another OS by
the repro job) before it serves the files; in the application-served mode (`WithPage`) the application
chooses what is served anyway. A host-supplied style is therefore either inside the pin (and
fixed per release) or outside it (and the pin no longer says what the approver sees).

What styling can do to this page, concretely: it cannot change what is signed (the signature binds
the digest the script computes from the arguments shown, and CSS cannot alter that), but it can
change what the approver is shown, which is the whole risk. A style outside the pin could hide the
digest line, restyle REJECT as the calm button, fake the origin banner, or make an Approve look like
the safe default.

| Option | What it is | Integrity consequence | Cost |
|---|---|---|---|
| **A. Excluded** | Keep `app.css` as is, add `forced-colors` and `prefers-contrast` rules and fix the audit items | None: the same files, a new hash at release as with any change | The page looks unlike the viewer; a bank cannot theme it; its palette stays hand-written and unchecked |
| **B. Shipped themes inside the bundle** | `app.css` gains the token layer and the three shipped themes (`light`, `dark`, `high-contrast`), selected by `prefers-color-scheme` and `prefers-contrast` only, plus the forced-colors mapping. The same validator and the same CI check run on them. No toggle, no `config.json` key, no URL input | The theme is part of `app.css`, so it is inside the SRI and the SHA256SUMS. The signed bytes still say exactly what the approver sees; the deployment's `config.json` still names only the intake. Repro keeps working (generated with the page build, deterministic) | About 3 KB more CSS (the bundle is about 43 KB). A bank that wants its colours has to fork the bundle, host its own, and pin its own hash, which is how it must treat any change to this page |
| **C. B plus a name and logo slot** | The deployment's `config.json` adds `brand: {name, logo}`; the page renders a validated name (text, at most 40 characters) and a logo (PNG, at most 16 KB, fixed box) in a fixed header region | `config.json` is not in the SUMS. The slot is data outside the pin that the page renders, so the host can show "Acme Compliance" and a logo above a call. In the application-served mode the application is the host and is the party the approver is meant to be protected from | The slot needs its own validation, a size and type check on a file the pin does not cover, a CSP `img-src` change if the logo is a separate file (or an inline data URL, which `img-src 'self'` forbids), and a rule that the slot is shown only when the page is hosted (`hosted`, which the page already computes) |

**Recommendation: B for v0.22.0.** It gives the approve page the same checked palette, the high-contrast
and forced-colors support it lacks, and a single source of colour, and it adds no input an attacker
could use: the host cannot choose a theme, a name, a colour or a length. The security property is a
sentence that stays true: "the pixels depend on the pinned bundle and the system's settings, and
nothing else". C is deferred until a deployment asks. If it is added, it is name only (no logo) first,
rendered by `textContent`, only when `hosted`, in a region that cannot overlap the call panel, and the
origin line stays beside it. The page also keeps its existing note that these checks are for
mistakes, not for a hostile page.

Two details for B. First, there is no toggle: a reader who wants dark sets the system to dark, so
there is no `localStorage` value to trust. Second, the page's `hosted` banner and the digest line use
the `alert` and `warning` tokens with a glyph, so the forced-colors ladder applies to them as well.

## Security

**CSS injection.** A theme holds no CSS. The parser produces typed values (a colour is three or six
hex digits, a length an integer in a range, an enum one word of a list); `theme.CSS` writes them in
canonical form into a fixed template. Nothing a user types is copied to the output except the theme
name, whose charset (`[a-z0-9-]`) cannot close a string or a block. There is no `url()`, `@import`,
`content`, `expression` or selector to write, so there is nothing for the stylesheet to fetch (a
second wall: `default-src 'self'` and `img-src 'self' data:` would stop it). A fuzz test feeds
arbitrary JSON to `Parse` and requires that whatever passes yields CSS matching a strict grammar
(a regular expression over the whole file); a mutation check removes the normalisation and the test
must fail.

**Overlay and hiding.** With no selectors and no layout properties, a theme cannot hide, move or
cover a control. What remains are values that make something unreadable, each closed by a check:
text matching its ground (C1), a border or focus ring that vanishes (C2), a status colour that merges
with another (C3, C4), a font size or spacing that pushes content off screen (the `type_scale` and
`density` ranges), a scrim that is nearly transparent or opaque (the `scrim` range), a font that
makes characters alike (an enum of generic families). The limits: the checks are numeric, not
perceptual proof (a theme can pass and still be ugly), and a custom theme can swap hues (ok red,
alert green) and pass; the words and glyphs still say what a state is, and decision 2 is whether to
add a polarity rule.

**A theme as an attack on the approval UI.** Three routes, and the answer to each. (1) A theme as the
input that changes the approve page: there is none; the page takes no theme input (option B). (2) A
theme that makes the viewer or the editor mislead the person who reads a verdict: the viewer is
read-only, the editor signs nothing, and both always draw the verdict word and glyph; a theme could at
worst use unusual hues, bounded by C3 and C4. (3) A theme supplied by an attacker who can already
write the operator's command line or the host's Go code: they hold more than a theme. A theme in a
flow file, a journal or a URL is not accepted, so a run or a flow author cannot restyle the
reviewer's screen. For an application-served approve page (`WithPage`), the application can already
serve any script; a theme feature adds no power there, and the page still says so on its face.

**Embedding.** A host passes only values that passed `Parse`. It cannot pass CSS, cannot add a
selector, and the generated stylesheet is served from the page's own origin under the unchanged CSP.
The host chooses the set, the default and the lock. It cannot hide the toggle's replacement for the
reader if the reader's system asks for forced colours or more contrast: those rules are not in the
theme.

Review: the "Security" and "The approve page" sections are to be read by an independent break-it
reviewer before the decisions are taken (see the pull request).

## Tests

- **Validator unit tests.** Table tests for every check, on both sides of each threshold. The
  contrast function against the published WCAG examples (`#767676` on white is 4.54 and passes,
  `#777777` is 4.48 and fails); CIEDE2000 against Sharma, Wu and Dalal's published 34 test pairs;
  the colour-blindness matrices against published reference values. Parser tests: unknown keys,
  duplicate keys, every forbidden colour form, an over-size file, an out-of-range length, a shipped
  name. A mutation check after the first commit: move a threshold by one step and a test must fail.
- **Shipped themes in CI.** A Go test loads every embedded theme, runs C1 to C7, and fails the build
  on C1 to C5 and C7 (C6 is printed). Adding a shipped theme adds a row.
- **Generator.** A golden CSS per shipped theme, and the grammar fuzz above.
- **No literal colours in page CSS.** A repository test (new, see decision 12) scans
  `internal/runview/assets/*.css`, `flowedit/page/*.css`, `approvals/page/app.css` for a hex or
  `rgb(` outside the shipped theme data, with a short allow-list (the tab icon, the Mermaid export).
  This is what keeps a later page from adding a colour that no theme checks.
- **Per-theme tests in Chrome**, in the existing browser jobs (puppeteer-core, pinned by lockfile):
  every page and state (the viewer's run, config, diff, attention and run list; the editor's five
  states; the approve page's panels) times every shipped theme and the example custom theme, with
  axe-core `color-contrast` and the rest of its rules, failing on any violation; and the computed
  colours of one element per semantic class equal the theme's tokens.
- **The colour-only audit as a test.** For each pair in A1 to A5 and A7 to A9, render both states
  with the Chrome DevTools vision-deficiency emulation set to achromatopsia (and forced-colors
  emulated) and require that their pixel regions differ; the allow-list is A10 to A14. The test also
  runs under `forced-colors: active` and `prefers-contrast: more` emulation, and asserts that the
  accessibility tree (`Accessibility.getFullAXTree`) is the same for every theme, so a theme cannot
  change what a screen reader is given.
- **The approve page.** Its existing tests run under every theme; the bundle hash test is updated;
  the repro job (`approvepage` rebuilt on another OS) stays as is, and a test asserts the page reads
  no theme input (no `config.json` key, no URL parameter, no storage value).
- **Manual, still owed.** A pass with VoiceOver, NVDA and Orca (already in KNOWN-LIMITATIONS) and
  now one with Windows High Contrast and macOS Increase contrast. The release notes say so until
  done. The tests above are not a substitute.

## Page size

Measured on `main`: the viewer's CSS is about 36 KB across its files; the editor's own files are
about 47 KB against a 150 KB budget; the approve bundle about 43 KB, of which `app.css` is 1.5 KB.

- Viewer: the dark palette is written twice in `style.css` today (about 1.1 KB each); that becomes
  one generated `theme.css` of three shipped themes (about 4 KB: 3 themes of about 45 values). Net
  about +2 KB. A custom theme adds about 1.3 KB.
- Editor: `editor.js` and `editor.css` change by tens of bytes (the toggle, `border_strong`,
  `on_accent`). Well inside the budget.
- Approve page: about +3 KB for the tokens, the three themes and the forced-colors block (about +7%
  of the bundle). No new file, so no new SRI attribute and no new entry in the SUMS.
- The Go side: a package of a few hundred lines (parser, checks, CSS, the two colour-science
  functions) and the shipped theme data. No dependency.

## Phasing and the pull-request split

The split keeps the phase 2 editor work (agent `editor-phase2`, in `flowedit/page/editor.js` and
`editor.css`) off the critical path. The rule that makes this work: **no CSS variable is renamed.**
Theme keys are mapped to the names the pages already use, and the new tokens (`info`, `on`,
`border_strong`, `on_accent`, `focus`, the syntax tokens) are additions with defaults that equal
today's values, so phase 2's CSS keeps working before and after. Phase 2 is told the one rule it must
follow now: use only the tokens in `style.css`, add no colour literal, and use `--line` only for
decorative borders.

| PR | What | Touches `flowedit/page`? |
|---|---|---|
| 1 | The `theme` package: parser, normaliser, checks C1 to C7, `CSS`, the shipped `light`, `dark` (today's values, so no visible change) and `high-contrast` as embedded JSON, the CI test, `bide theme check`. No page changes | No |
| 2 | The viewer on generated tokens: `theme.css` served and the three palette blocks removed from `style.css`; `theme.js` and the toggle learn three themes and the remembered-name check; `-theme` and `-theme-default` on `bide view`; the Chrome matrix over the viewer's pages (screenshots must match the previous output for light and dark); the guide `docs/guides/themes.md` | No (`style.css` is shared but is the viewer's) |
| 3 | The colour-only fixes A1 to A5, A7 to A9 in `graph.css`, `diff.css`, `extras.css`, `svg.go`, `live.js`; the retuned `idem`; the audit test | No |
| 4 | `forced-colors` and `prefers-contrast`: the mapping, the shape ladder completed, `border_strong` on every control in the viewer's CSS, the `info` token adopted where the pages use `accent` for a state; the high-contrast theme goes live | No |
| 5 | The approve page: option B, the same tests, the bundle hash test, a release-notes line saying the page's hash changes | No |
| 6 | The editor adopts it: `editor.css` and `editor.js` (the toggle function, `.btn.primary` to `on_accent`, the control borders to `border_strong`, the `.node.sel` ring to `focus`), `-theme` on `bide edit`, the per-theme Chrome states | **Yes**, about 30 changed lines; the PR waits for a quiet moment in phase 2 and rebases on it (rebase, force-push with lease, no update-branch) |
| 7 | The Go embedding option on the public handlers, with its test and its documentation | The editor's handler option only, in Go, not in the page |
| 8 | Docs: KNOWN-LIMITATIONS (the manual forced-colors pass), the editor design note's accessibility paragraph, the viewer and editor guides, the i18n copies of what changed | No |

Each PR carries the docs and changelog fragment for what it changes, as the repository requires; PR
8 is what is left (cross-links and the i18n copies). PRs 3, 4 and 5 are independent of one another
after PR 2 and can run in parallel. Cut line for v0.22.0 if time is short: PR 7 (the Go option) slips
first, then PR 5; PRs 1 to 4 and 6 are what makes the pages themeable and accessible. The size
estimate for the whole series is about 1,500 lines of Go with tests, 400 lines of CSS and 100 of JS.

## Decisions for the maintainer

Each has a recommendation. Items marked **new** introduce a practice or a public surface the project
does not have today.

1. **Do not rename the CSS variables.** A rename-first PR was considered; this note
   recommends against it. The theme file uses friendly keys that map to the existing names, so
   `editor.css` and the other pages are untouched until they adopt the new tokens. A rename can
   follow later as a mechanical PR if wanted. Recommend: no rename.
2. **Semantic colours are themable, with no hue rule.** A custom theme may pick any hues that pass
   C1 to C5; there is no rule that alert must be red. A hue rule blocks colour-blind-safe palettes
   (blue and orange); the words, glyphs and shapes are fixed and carry the meaning. The approve page
   takes no custom theme, so its REJECT and Approve are never reachable by this. Recommend: no hue
   rule (the alternative is a polarity check that alert is warm and ok is not).
3. **Named meanings (path taken, refused, weakened) are aliases of the eight colour tokens, not extra
   colours.** Recommend: aliases.
4. **Add an `info` token separate from `accent`** (defaults equal), so "waiting" and "path taken" are
   not the same value as "a link". Recommend: yes.
5. **No per-kind colours on the canvas**, only `canvas_bg` and `node_fill`: kinds are told apart by
   icon and label and colour belongs to state and risk. Recommend: yes.
6. **The thresholds:** text 4.5:1, non-text 3:1, status colours at least 20 apart (CIEDE2000), domain
   colours at least 10, WCAG 2.x as the metric; the colour-blindness check (at least 8 under the three
   simulations) advisory in v0.22.0, since the current light and dark themes fail it. Also: retune
   `idem` so it is not the same blue as `info`. Recommend: as stated; revisit C6 as a hard check when
   the palette is next changed on purpose.
7. **The format and the package.** JSON, parsed by a new public root-module package, 0.x API; fonts an
   enum of generic families (no named families, no web fonts). **New** public package. Recommend: yes.
8. **Custom themes by file for the viewer and the editor:** `-theme` and `-theme-default`, one custom
   theme per process, `bide theme check` as a command. **New** subcommand. Recommend: yes.
9. **The approve page takes option B** (shipped themes inside the signed bundle, system-selected, no
   toggle, no host input); the name-and-logo slot is deferred. Recommend: B; C only when asked, and
   then name-only and only when hosted.
10. **forced-colors: the system wins**, `forced-color-adjust: none` is never used, and the shape ladder
    is a requirement on the pages. Recommend: yes.
11. **Shipped themes: `light`, `dark`, `high-contrast`.** The "neutral example" is a custom-theme
    example file in the docs, tested in CI, not a fourth shipped theme. `prefers-contrast: more`
    selects `high-contrast` unless the reader chose. Recommend: yes.
12. **A repository test that forbids colour literals in page CSS**, with a short allow-list. **New**
    practice: it changes what a page change (phase 2 included) may do. Recommend: yes, from PR 2.
13. **Fix the colour-only items (A1 to A5, A7 to A9) before themes ship**, which changes how the
    existing graph looks (edge styles, the warning mark). Recommend: yes, in PR 3.
14. **Scope of v0.22.0:** PRs 1 to 6 and 8, with PR 7 (the Go embedding option) the first to slip.
    The manual screen-reader and forced-colors pass stays owed and is named in the release notes.
    Recommend: as stated.
