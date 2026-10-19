// bide edit: the page. It holds the config text and draws what the server computes; it validates
// nothing itself. Config text is untrusted: every name, comment and message goes into the page with
// textContent or a property, never as markup, and the graph (SVG the server escapes) is parsed with
// DOMParser and adopted.
//
// Request and response shapes (the Go side: flowedit.Doc and flowedit.View):
//   POST /api/doc    {}             -> Doc  { ...View, hash }
//   POST /api/report {text}         -> View { name, format, text, mode, parse_error, flow, entry,
//                                             verdict{class,word,icon,line,details,stats[]}, errors, warnings,
//                                             problems[], assertions[], nodes[], wiring[], graph,
//                                             weakening{baseline, weakened[], word, line, strengthened[], changes[], compare_error} }
//   POST /api/preview {text, hash}  -> { diff, removed_comments[], changed, hash, conflict, needs_confirmation[], errors, warnings }
//   POST /api/save {text, hash, confirmed[]} -> Doc, or 409 { conflict, hash, diff }, or 422 { needs_confirmation[] }
// Every call is a POST with the session token (from the URL fragment, #t=...) in X-Bide-Token.

const SVGNS = 'http://www.w3.org/2000/svg';

const token = (/(?:^#|&)t=([0-9a-f]+)/.exec(location.hash) || [])[1] || '';

// shown is text with every character that does not print written as \u{XXXX} (a line break and a tab
// stay): a name in a hand-written file can hold a direction override or a zero-width character (the
// loader accepts any string), and the page must not draw one as another name. It is applied at the
// choke points, so no surface can forget it: h() (every text child, and the attributes a reader or a
// screen reader reads: aria-label, title, placeholder, alt), replace(), announce(), and the drawing's
// text, titles and labels. The inputs a name is edited in, the source and option values keep the raw
// text.
const NONPRINT = /[^\p{L}\p{M}\p{N}\p{P}\p{S} \t\n\r]|[\p{Default_Ignorable_Code_Point}\u2800]|^\p{M}/gu;
export const shown = (s) => String(s).replace(NONPRINT, (c) => '\\u{' + c.codePointAt(0).toString(16).toUpperCase() + '}');

// h builds an element: h('p', {class: 'x'}, 'text', child).
const READ_ATTRS = new Set(['aria-label', 'title', 'placeholder', 'alt', 'aria-description']);
export function h(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    e.setAttribute(k, v === true ? '' : READ_ATTRS.has(k) ? shown(v) : String(v));
  }
  for (const k of kids.flat()) {
    if (k == null || k === false) continue;
    e.append(typeof k === 'string' ? document.createTextNode(shown(k)) : k);
  }
  return e;
}

export function icon(name) {
  const svg = document.createElementNS(SVGNS, 'svg');
  svg.setAttribute('class', 'ic ic-' + name);
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const use = document.createElementNS(SVGNS, 'use');
  use.setAttribute('href', '#ic-' + name);
  svg.append(use);
  return svg;
}

// applyOp asks the server to apply an operation to the text. It resolves with the new text's view
// ({text, changed, ...}) and rejects with the server's reason when the operation cannot be applied.
async function applyOp(text, operation) {
  const e = await api('/api/apply', { text, operation });
  if (e.refused) throw new Error(e.refused);
  return e;
}

async function api(path, body = {}) {
  busy(1);
  try {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Bide-Token': token },
      body: JSON.stringify(body),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw Object.assign(new Error(data.error || res.status + ' ' + res.statusText), { status: res.status, data });
    return data;
  } finally {
    busy(-1);
  }
}

// ---- at rest ----
//
// <html data-busy> is the number of things the page has started and not finished: requests in flight
// and the pauses it waits out before it asks the server (the check of what was typed, of a gate draft,
// of an assertion). It is "0" when the page has nothing under way, so a test, or a tool that drives
// the page, waits for that and not for a guess of how long a redraw takes.
let busyN = 0;
function busy(d) {
  busyN += d;
  document.documentElement.dataset.busy = String(busyN);
}
document.documentElement.dataset.busy = '0';

// pause is one pending timer: set replaces the one before it, and the pause counts as busy until its
// function has finished.
function pause() {
  let t = 0;
  let pending = false;
  return {
    cancel() { if (pending) { clearTimeout(t); pending = false; busy(-1); } },
    set(fn, ms) {
      this.cancel();
      pending = true;
      busy(1);
      t = setTimeout(async () => {
        pending = false;
        try { await fn(); } finally { busy(-1); }
      }, ms);
    },
  };
}

const $ = (id) => document.getElementById(id);
const replace = (id, ...kids) => $(id).replaceChildren(...kids.flat().filter(Boolean).map((k) => (typeof k === 'string' ? shown(k) : k)));
// announce speaks through the live region. The same words twice in a row (Undone, Undone) must be
// heard twice, and a region whose text does not change is not announced, so a no-break space is
// alternated on the end.
let lastSpoken = '';
let flip = false;
const announce = (msg) => {
  flip = msg === lastSpoken ? !flip : false;
  lastSpoken = msg;
  $('live').textContent = shown(msg) + (flip ? '\u00a0' : '');
};
// A verdict is announced when it changes, not on every keystroke that leaves it as it was.
let lastVerdict = '';

let view = null;
let selected = '';

export function renderVerdict(v) {
  const d = v.verdict;
  const sec = h('section', { class: 'verdict v-' + d.class, 'aria-labelledby': 'vword' },
    h('div', { class: 'vicon' }, icon(d.icon)),
    h('div', { class: 'vtext' },
      h('p', { class: 'vword', id: 'vword' }, d.word),
      h('p', { class: 'vline' }, d.line),
      (d.details || []).map((x) => h('p', { class: 'vdetail' }, x))));
  const stats = (d.stats || []).length === 0 ? null : h('ul', { class: 'stats' },
    d.stats.map((s) => h('li', { class: 'stat' + (s.class ? ' s-' + s.class : '') },
      h('span', { class: 'num' }, s.value), h('span', { class: 'lbl' }, s.label), s.sub ? h('span', { class: 'sub' }, s.sub) : null)));
  replace('verdict', sec, stats);
}

// The weakening banner is the classifier's answer (bide check -diff's) for this text against the
// baseline. It has no close control: a banner that can be dismissed is a warning that can be missed.
// It is gone only when the text no longer weakens, and it speaks (role alert) when it appears.
let lastBanner = '';
export function renderWeakening(v) {
  const w = v.weakening;
  if (!w) { replace('weakened'); replace('baseline'); lastBanner = ''; return; }
  // An alert is spoken when it appears. Drawing the same banner again on every keystroke would speak
  // it again each time, so a banner that has not changed is left as it is.
  const key = JSON.stringify([w.weakened, w.word, w.line, w.compare_error, w.baseline]);
  if (key === lastBanner) {
    renderBaselineLine(w);
    return;
  }
  lastBanner = key;
  if (w.weakened && w.weakened.length) {
    replace('weakened', h('section', { class: 'verdict v-bad weakened', role: 'alert', 'aria-labelledby': 'wword' },
      h('div', { class: 'vicon' }, icon('halt')),
      h('div', { class: 'vtext' },
        h('p', { class: 'vword', id: 'wword' }, w.word || 'WEAKENED'),
        h('p', { class: 'vline' }, (w.line || 'This is weaker than the baseline') + ', compared with ' + w.baseline + '.'),
        w.weakened.map((x) => h('p', { class: 'vdetail' }, x)))));
  } else if (w.compare_error) {
    replace('weakened', h('section', { class: 'verdict v-warn weakened', role: 'alert', 'aria-labelledby': 'wword' },
      h('div', { class: 'vicon' }, icon('unknown')),
      h('div', { class: 'vtext' },
        h('p', { class: 'vword', id: 'wword' }, 'NOT COMPARED'),
        h('p', { class: 'vline' }, 'Whether this is weaker than ' + w.baseline + ' cannot be said: ' + w.compare_error))));
  } else {
    replace('weakened');
  }
  renderBaselineLine(w);
}

function renderBaselineLine(w) {
  const word = !(w.weakened && w.weakened.length) && !w.compare_error ? 'not weaker than' : 'compared with';
  const ch = (w.changes || []).length;
  replace('baseline', 'Baseline: ' + word + ' ' + w.baseline + (ch ? ' (' + ch + ' change' + (ch === 1 ? '' : 's') + ')' : ' (no changes)') +
    ((w.strengthened || []).length ? '; stronger in ' + w.strengthened.length + ' way' + (w.strengthened.length === 1 ? '' : 's') : '') + '.');
}

// The outline is the primary accessible view: a list in config order whose items are buttons. One
// item is in the tab order (the selected one, else the first) and the arrow keys, Home and End move
// between them, so a long outline is one Tab stop; Enter or Space selects.
export function renderOutline(v) {
  const nodes = v.nodes || [];
  const stop = Math.max(0, nodes.findIndex((n) => n.name === selected));
  replace('outline', nodes.map((n, i) => {
    const b = h('button', { type: 'button', 'data-node': n.name, 'aria-current': n.name === selected ? 'true' : 'false', tabindex: i === stop ? '0' : '-1' },
      h('span', { class: 'nm' }, shown(n.name)),
      h('span', { class: 'meta' }, [n.kind, n.block, n.risk, n.gate ? 'gate ' + n.gate : ''].filter(Boolean).join(' · ')),
      (n.problems || []).map((p) => h('span', { class: 'prob' }, p)));
    b.addEventListener('click', () => select(n.name));
    return h('li', {}, b);
  }));
  const wiring = v.wiring || [];
  if (selectedWire >= wiring.length) selectedWire = -1;
  const wstop = Math.max(0, selectedWire);
  replace('wiring', wiring.map((w, i) => {
    const b = h('button', { type: 'button', 'data-wire': String(i), 'aria-current': i === selectedWire ? 'true' : 'false', tabindex: i === wstop ? '0' : '-1' }, w);
    b.addEventListener('click', () => selectWire(i));
    return h('li', {}, b);
  }));
  renderWireMoves(wiring);
  renderFlowSettings(v);
  renderAddJoin(v);
  replace('an-blocks', [...new Set(nodes.map((x) => x.block).filter(Boolean))].filter((b) => shown(b) === b).map((b) => h('option', { value: b })));
}

// A join is added from a disclosure: a name, a merge block and the inputs, chosen from the nodes in
// the order they are ticked. The checkboxes are drawn again only when the nodes change, so what is
// typed or ticked is not lost to an unrelated redraw.
function renderAddJoin(v) {
  const box = $('add-join');
  if (!box) return;
  const names = (v.nodes || []).map((n) => n.name);
  const key = names.join('\u0000');
  if (box.dataset.names === key && box.dataset.drawn) return;
  box.dataset.names = key;
  box.dataset.drawn = '1';
  const name = h('input', { id: 'aj-name', type: 'text', autocomplete: 'off', spellcheck: 'false' });
  const merge = h('input', { id: 'aj-merge', type: 'text', autocomplete: 'off', spellcheck: 'false', 'aria-describedby': 'aj-help' });
  const boxes = names.map((x, i) => {
    const cb = h('input', { id: 'aj-in-' + i, type: 'checkbox', value: x });
    return h('div', { class: 'field check' }, cb, h('label', { for: 'aj-in-' + i }, x));
  });
  const add = h('button', { type: 'button', class: 'btn', id: 'aj-add' }, 'Add join');
  add.addEventListener('click', async () => {
    const inputs = [...box.querySelectorAll('input[type=checkbox]:checked')].map((c) => c.value);
    const to = name.value.trim();
    if (await operate({ op: 'add_join', node: to, value: merge.value.trim(), inputs }, 'j-merge', to)) {
      name.value = '';
      merge.value = '';
      for (const c of box.querySelectorAll('input[type=checkbox]')) c.checked = false;
    }
  });
  box.replaceChildren(h('summary', {}, 'Add a join'),
    h('div', { class: 'field' }, h('label', { for: 'aj-name' }, 'Name'), name),
    h('div', { class: 'field' }, h('label', { for: 'aj-merge' }, 'Merge block'), merge),
    h('fieldset', { class: 'inputs' }, h('legend', {}, 'Inputs'), boxes),
    h('p', { class: 'muted small', id: 'aj-help' }, 'A join merges the results of its inputs. The ticked nodes become its inputs in the order the nodes are listed; the order can be changed on the join afterwards. Its merge block is a name a program registers.'),
    h('p', { id: 'aj-error', class: 'bad small', role: 'alert' }), add);
}

// The flow's name and its documentation type names, as one small form under the outline.
function renderFlowSettings(v) {
  const box = $('flow-settings');
  if (!box || box.dataset.flow === (v.flow || '') + '\u0000' + (v.in || '') + '\u0000' + (v.out || '') && box.dataset.drawn) return;
  box.dataset.flow = (v.flow || '') + '\u0000' + (v.in || '') + '\u0000' + (v.out || '');
  box.dataset.drawn = '1';
  const row = (id, label, value, op, help) => {
    const input = h('input', { id, type: 'text', value: value || '', autocomplete: 'off', spellcheck: 'false', 'aria-describedby': id + '-help' });
    const set = h('button', { type: 'button', class: 'btn', id: id + '-set' }, 'Set');
    const go = () => operate({ op, value: input.value.trim() }, id);
    set.addEventListener('click', go);
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); go(); } });
    return h('div', { class: 'field' }, h('label', { for: id }, label), h('div', { class: 'inline' }, input, set), h('p', { class: 'muted small', id: id + '-help' }, help));
  };
  box.replaceChildren(
    h('summary', {}, 'Flow name and types'),
    row('fs-flow', 'Name', v.flow, 'set_flow', 'The flow\'s label: it is in the topology digest, so renaming it changes the digest.'),
    row('fs-in', 'Input type (documentation)', v.in, 'set_in', 'Optional. A type name that must match the Go input type when a program loads the flow; empty removes it.'),
    row('fs-out', 'Output type (documentation)', v.out, 'set_out', 'Optional, like the input type.'));
}

// The wiring elements are a list like the outline: one Tab stop, arrow keys between them, Enter
// selects. The one pair of Move buttons below it acts on the selected element, so the list costs three
// Tab stops however long it is.
let selectedWire = -1;
function selectWire(i) {
  selectedWire = i;
  for (const b of $('wiring').querySelectorAll('button')) b.setAttribute('aria-current', b.getAttribute('data-wire') === String(i) ? 'true' : 'false');
  renderWireMoves(view.wiring || []);
  announce('Selected wiring element ' + (i + 1));
}

function renderWireMoves(wiring) {
  const i = selectedWire;
  const mv = (dir) => {
    const b = h('button', { type: 'button', class: 'btn', id: 'w-' + dir, disabled: i < 0 || (dir === 'up' ? i === 0 : i === wiring.length - 1), 'aria-label': i < 0 ? 'Move ' + dir + ': no wiring element selected' : 'Move ' + dir + ': wiring element ' + (i + 1) }, 'Move ' + dir);
    b.addEventListener('click', () => {
      const to = dir === 'up' ? i - 1 : i + 1;
      const edge = dir === 'up' ? to === 0 : to === wiring.length - 1;
      selectedWire = to;
      operate({ op: 'move_wiring', index: i, value: dir }, edge ? (dir === 'up' ? 'w-down' : 'w-up') : 'w-' + dir);
    });
    return b;
  };
  replace('wire-moves',
    h('p', { class: 'small', id: 'w-pos' }, i < 0 ? 'Select a wiring element to move it.' : 'Wiring element ' + (i + 1) + ' of ' + wiring.length + ' is selected.'),
    h('div', { class: 'inline' }, mv('up'), mv('down')));
}

// ---- skip links (WCAG 2.4.1): the page has many controls between the regions ----
//
// A skip link moves the focus into the region it names: the toolbar's first enabled control (Save,
// when there is something to save), the outline's tab stop, the inspector's first control, or the
// heading of the assertions (a heading made focusable for this). The links are hidden until they
// have the focus.
function skipTo(target) {
  const first = (sel) => document.querySelector(sel);
  const el = {
    toolbar: () => first('#toolbar button:not(:disabled)'),
    outline: () => first('#outline button[tabindex="0"]') || $('h-outline'),
    inspector: () => first('#inspector select, #inspector input, #inspector button:not(:disabled)') || $('h-inspector'),
    assertions: () => $('h-assert'),
  }[target];
  const to = el && el();
  if (to) to.focus();
}

function wireSkipLinks() {
  for (const a of document.querySelectorAll('a[data-skip]')) {
    a.addEventListener('click', (e) => { e.preventDefault(); skipTo(a.getAttribute('data-skip')); });
  }
}

function outlineKeys(e) {
  const items = [...e.currentTarget.querySelectorAll('button')];
  const at = items.indexOf(document.activeElement);
  if (at < 0) return;
  const to = { ArrowDown: Math.min(at + 1, items.length - 1), ArrowUp: Math.max(at - 1, 0), Home: 0, End: items.length - 1 }[e.key];
  if (to === undefined) return;
  e.preventDefault();
  for (const b of items) b.setAttribute('tabindex', '-1');
  items[to].setAttribute('tabindex', '0');
  items[to].focus();
}

// ---- the inspector: a node's block, safety and approval gate, as forms ----
//
// A form change is an operation sent to the server (POST /api/apply {text, operation}); the answer
// is the new text and its check, which the page shows. The gate form edits a draft: each change to
// it asks the server what the check would say of the node with that gate (the same call, not kept),
// and shows the node's problems under the form, so what the form rejects is what bide check
// rejects. Apply writes the draft into the text.
//   POST /api/apply {text, operation{op, node, value, approval{need, approvers[], exclude_principal, timeout}, assertion{form, node, via, k, n, excluding}, index}}
//     -> View + { changed }, or { refused: reason } when the operation cannot be applied

const SAFETIES = [['', 'left to the block'], ['readonly', 'readonly: no external effect, a resume re-runs it'], ['idempotent', 'idempotent: safe to repeat, a resume re-runs it'], ['side_effect', 'side_effect: must not run twice, a resume halts and asks']];

function field(id, label, control, help) {
  return h('div', { class: 'field' }, h('label', { for: id }, label), control, help ? h('p', { class: 'muted small', id: id + '-help' }, help) : null);
}

export function renderInspector(v) {
  const n = (v.nodes || []).find((x) => x.name === selected);
  if (!n) {
    replace('inspector', h('p', { class: 'muted' }, 'Select a node in the flow or the outline.'));
    return;
  }
  const isJoin = n.kind === 'join';
  const row = (k, val) => val ? [h('dt', {}, k), h('dd', {}, val)] : [];
  // A suggestion is offered only when it prints: a datalist cannot show an escaped value and insert the raw one.
  const blocks = [...new Set((v.nodes || []).map((x) => x.block).filter(Boolean))].filter((b) => shown(b) === b);
  const safety = h('select', { id: 'f-safety' }, SAFETIES.map(([val, label]) => h('option', { value: val, selected: val === (n.safety || '') }, label)));
  safety.addEventListener('change', () => operate({ op: 'set_safety', node: n.name, value: safety.value }, 'f-safety'));
  const block = h('input', { id: 'f-block', type: 'text', value: n.block || '', list: 'f-blocks', autocomplete: 'off', spellcheck: 'false' });
  const setBlock = h('button', { type: 'button', class: 'btn', id: 'f-block-set' }, 'Set block');
  setBlock.addEventListener('click', () => operate({ op: 'set_block', node: n.name, value: block.value.trim() }, 'f-block'));
  block.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); setBlock.click(); } });
  replace('inspector',
    h('h3', {}, shown(n.name)),
    shown(n.name) !== n.name ? h('p', { class: 'warn small', id: 'f-name-escaped' }, 'This name has characters that do not print, written out here as \\u{code}. Rename it.') : null,
    h('dl', {}, row('kind', n.kind), row('crash behavior', n.risk), row('approval gate', n.gate || 'none'), row('problems', (n.problems || []).join('; '))),
    h('p', { id: 'op-error', class: 'bad small', role: 'alert' }),
    h('div', { class: 'form' },
      field('f-safety', 'Safety', safety, 'Only lowers what the block declares in Go; the check cannot see that here, so a value above it fails when a program loads the flow.'),
      isJoin ? null : h('div', { class: 'field' }, h('label', { for: 'f-block' }, 'Block'), h('div', { class: 'inline' }, block, setBlock),
        h('datalist', { id: 'f-blocks' }, blocks.map((b) => h('option', { value: b }))),
        h('p', { class: 'muted small' }, 'A block name is chosen, not written: its Go code is registered in your program.')),
      isJoin ? joinForm(n, v) : null,
      gateForm(n),
      isJoin ? null : structureForm(n, v),
      connectionsForm(n, v)));
}

// ---- joins: a fan-in with its inputs, in order, and its merge block ----
//
// A join is a wiring element, not a declared node. Its inputs are the nodes whose results it
// merges, in the order its merge block takes them; the merge block is a name a program registers. The
// editor checks what the config says (an input is a declared node or join, named once); the arity and
// the types are checked when a program loads the flow.

function joinForm(n, v) {
  const w = (v.wires || []).find((x) => x.kind === 'join' && x.join === n.name) || { inputs: [], merge: '' };
  const inputs = w.inputs || [];
  const all = (v.nodes || []).map((x) => x.name).filter((x) => x !== n.name && !inputs.includes(x));
  const send = (list, focus) => operate({ op: 'set_join_inputs', node: n.name, inputs: list }, focus);
  const merge = h('input', { id: 'j-merge', type: 'text', value: w.merge || '', autocomplete: 'off', spellcheck: 'false', 'aria-describedby': 'j-merge-help' });
  const setMerge = h('button', { type: 'button', class: 'btn', id: 'j-merge-set' }, 'Set merge');
  const doMerge = () => operate({ op: 'set_join_merge', node: n.name, value: merge.value.trim() }, 'j-merge');
  setMerge.addEventListener('click', doMerge);
  merge.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); doMerge(); } });
  const btn = (id, text, label, onClick, disabled) => {
    const b = h('button', { type: 'button', class: 'btn', id, 'aria-label': label, disabled: !!disabled }, text);
    b.addEventListener('click', onClick);
    return b;
  };
  const rows = inputs.map((x, i) => {
    const swap = (to) => { const l = [...inputs]; [l[i], l[to]] = [l[to], l[i]]; return l; };
    const up = btn('j-in-up-' + i, 'Up', 'Move up: input ' + (i + 1) + ', ' + x, () => send(swap(i - 1), i - 1 === 0 ? 'j-in-down-0' : 'j-in-up-' + (i - 1)), i === 0);
    const down = btn('j-in-down-' + i, 'Down', 'Move down: input ' + (i + 1) + ', ' + x, () => send(swap(i + 1), i + 1 === inputs.length - 1 ? 'j-in-up-' + (i + 1) : 'j-in-down-' + (i + 1)), i === inputs.length - 1);
    const rm = btn('j-in-rm-' + i, 'Remove', 'Remove input ' + (i + 1) + ', ' + x, () => send(inputs.filter((_, k) => k !== i), 'j-merge'), inputs.length === 1);
    return h('li', {}, h('span', {}, (i + 1) + '. ' + x), ' ', up, down, rm);
  });
  const addSel = h('select', { id: 'j-in-new' }, all.map((x) => h('option', { value: x }, x)));
  const remove = btn('j-remove', 'Remove join', 'Remove the join ' + n.name, () => removeNode(n, remove, 'remove_join'));
  return h('fieldset', { class: 'structure', id: 'join-form' },
    h('legend', {}, 'Join'),
    h('div', { class: 'field' }, h('label', { for: 'j-merge' }, 'Merge block'), h('div', { class: 'inline' }, merge, setMerge),
      h('p', { class: 'muted small', id: 'j-merge-help' }, 'A name a program registers (RegisterJoin2 or RegisterJoin3). The editor cannot check that it is registered or that its arity and types fit the inputs; loading the flow does.')),
    h('ol', { class: 'conns', id: 'j-inputs', 'aria-label': 'Inputs of ' + n.name }, rows),
    h('div', { class: 'field' }, h('label', { for: 'j-in-new' }, 'Add an input'), addSel),
    h('div', { class: 'inline' }, btn('j-in-add', 'Add input', 'Add an input to ' + n.name, () => send([...inputs, $('j-in-new').value], 'j-in-new'), !all.length), remove));
}

// ---- connections: the edges out of a node, its switch, arms and else ----
//
// A node has edges out or a switch, not both (plan.Load refuses both), so the form offers one or
// the other. Targets are chosen from the declared nodes, never typed, so an edge to a node that is not
// there cannot be written; a predicate is a name a program registers, typed.

function pick(id, label, names, value, { none } = {}) {
  const sel = h('select', { id }, none ? h('option', { value: '' }, none) : null,
    names.map((x) => h('option', { value: x, selected: x === value }, x)));
  return field(id, label, sel);
}

function connectionsForm(n, v) {
  const wires = v.wires || [];
  const names = (v.nodes || []).map((x) => x.name);
  const others = names.filter((x) => x !== n.name);
  const joins = new Set(wires.filter((w) => w.kind === 'join').map((w) => w.join));
  const targets = others.filter((x) => !joins.has(x));
  const outs = wires.filter((w) => w.kind === 'edge' && w.from === n.name);
  const ins = wires.filter((w) => w.kind === 'edge' && w.to === n.name);
  const isJoin = n.kind === 'join';
  const sw = isJoin ? null : wires.find((w) => w.kind === 'switch' && w.over === n.name);
  const val = (id) => $(id).value;
  const btn = (id, text, label, onClick, disabled) => {
    const b = h('button', { type: 'button', class: 'btn', id, 'aria-label': label, disabled: !!disabled }, text);
    b.addEventListener('click', onClick);
    return b;
  };
  const kids = [h('legend', {}, 'Connections')];
  if (!isJoin) kids.push(h('p', { class: 'small', id: 'c-in' }, ins.length ? 'Edges in: ' + ins.map((w) => w.from).join(', ') + '.' : 'No edge leads here.'));
  if (!sw) {
    kids.push(h('ul', { class: 'conns', id: 'c-outs', 'aria-label': 'Edges out of ' + n.name },
      outs.map((w, i) => h('li', {}, h('span', {}, n.name + ' \u2192 ' + w.to), ' ',
        btn('c-rm-edge-' + i, 'Remove', 'Remove edge: ' + n.name + ' to ' + w.to, () => operate({ op: 'remove_edge', node: n.name, to: w.to }, 'c-edge-to'))))));
    if (!outs.length) kids.push(h('p', { class: 'muted small' }, 'No edge leaves this node.'));
    kids.push(pick('c-edge-to', 'Add an edge to', targets, targets[0]));
    kids.push(h('div', { class: 'inline' }, btn('c-edge-add', 'Add edge', 'Add edge from ' + n.name, () => operate({ op: 'add_edge', node: n.name, to: val('c-edge-to') }, 'c-edge-to'), !targets.length)));
  }
  if (sw) {
    const arms = sw.arms || [];
    const rows = arms.map((a, i) => {
      const pred = h('input', { id: 'c-arm-pred-' + i, type: 'text', value: a.pred, autocomplete: 'off', spellcheck: 'false', 'aria-label': 'Predicate of arm ' + (i + 1) });
      const to = h('select', { id: 'c-arm-to-' + i, 'aria-label': 'Target of arm ' + (i + 1) }, targets.map((x) => h('option', { value: x, selected: x === a.to }, x)));
      const move = (dir) => btn('c-arm-' + dir + '-' + i, dir === 'up' ? 'Up' : 'Down', 'Move up: arm ' + (i + 1), () => {
        const to = dir === 'up' ? i - 1 : i + 1;
        const edge = dir === 'up' ? to === 0 : to === arms.length - 1;
        operate({ op: 'move_arm', node: n.name, index: i, value: dir }, 'c-arm-' + (edge ? (dir === 'up' ? 'down' : 'up') : dir) + '-' + to);
      }, dir === 'up' ? i === 0 : i === arms.length - 1);
      const down = move('down'); down.setAttribute('aria-label', 'Move down: arm ' + (i + 1));
      const max = h('input', { id: 'c-arm-max-' + i, type: 'number', min: '0', step: '1', value: String(a.loop_max || 0), 'aria-label': 'Loop back at most, arm ' + (i + 1) + ' (0: an ordinary arm)' });
      const setMax = () => { const raw = max.value.trim(); if (!/^\d+$/.test(raw)) { announce('Loop bound must be a whole number such as 3, or 0.'); return; } operate({ op: 'set_loop_max', node: n.name, index: i, max: Number(raw) }, 'c-arm-max-' + i); };
      max.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); setMax(); } });
      return h('li', { class: 'arm' }, h('span', { class: 'sr-only' }, 'Arm ' + (i + 1) + (a.loop_max ? ', loop-back' : '') + ': '), pred, ' \u2192 ', to, ' ', h('label', { for: 'c-arm-max-' + i, class: 'small' }, 'loops at most'), max, btn('c-arm-maxset-' + i, 'Set loop', 'Set the loop bound of arm ' + (i + 1), setMax), ' ',
        btn('c-arm-set-' + i, 'Set', 'Set arm ' + (i + 1), () => operate({ op: 'set_arm', node: n.name, index: i, pred: val('c-arm-pred-' + i).trim(), to: val('c-arm-to-' + i) }, 'c-arm-set-' + i)),
        move('up'), down,
        btn('c-arm-rm-' + i, 'Remove', 'Remove arm ' + (i + 1), () => operate({ op: 'remove_arm', node: n.name, index: i }, 'c-arm-pred-' + Math.max(0, i - 1)), arms.length === 1));
    });
    kids.push(h('p', { class: 'small' }, n.name + ' is switched over: the first arm whose predicate holds routes the value. It has arms instead of edges. A loop bound belongs on an arm that goes back to an earlier node of the loop, and the loop needs an exit (another arm or an else); the editor refuses a bound that loading the flow would refuse.'));
    kids.push(h('ol', { class: 'conns', id: 'c-arms', 'aria-label': 'Arms of the switch on ' + n.name }, rows));
    kids.push(h('div', { class: 'field' }, h('label', { for: 'c-arm-pred' }, 'Add an arm: predicate'), h('input', { id: 'c-arm-pred', type: 'text', autocomplete: 'off', spellcheck: 'false' })));
    kids.push(pick('c-arm-to', 'Add an arm: target', targets, targets[0]));
    kids.push(h('div', { class: 'inline' }, btn('c-arm-add', 'Add arm', 'Add arm to the switch on ' + n.name, () => operate({ op: 'add_arm', node: n.name, pred: val('c-arm-pred').trim(), to: val('c-arm-to') }, 'c-arm-pred'), !targets.length)));
    kids.push(pick('c-else', 'Else (when no arm holds)', targets, sw.else || '', { none: 'no else' }));
    kids.push(h('div', { class: 'inline' }, btn('c-else-set', 'Set else', 'Set the else of the switch on ' + n.name, () => operate({ op: 'set_else', node: n.name, to: val('c-else') }, 'c-else'))));
    kids.push(h('div', { class: 'inline' }, btn('c-sw-rm', 'Remove switch', 'Remove the switch on ' + n.name, () => operate({ op: 'remove_switch', node: n.name }, 'c-edge-to'))));
    if (arms.length === 1) kids.push(h('p', { class: 'muted small' }, 'The only arm cannot be removed; remove the switch instead.'));
  } else if (!outs.length && !isJoin) {
    kids.push(h('div', { class: 'field' }, h('label', { for: 'c-sw-pred' }, 'Make this a switch: first predicate'), h('input', { id: 'c-sw-pred', type: 'text', autocomplete: 'off', spellcheck: 'false' })));
    kids.push(pick('c-sw-to', 'Make this a switch: first target', targets, targets[0]));
    kids.push(h('div', { class: 'inline' }, btn('c-sw-add', 'Make a switch', 'Make ' + n.name + ' a switch', () => operate({ op: 'add_switch', node: n.name, pred: val('c-sw-pred').trim(), to: val('c-sw-to') }, 'c-sw-pred'), !targets.length)));
  }
  kids.push(h('p', { class: 'muted small' }, 'You can also drag from the small circle at a node\'s right edge in the flow onto another node to add an edge. The gate check follows paths: a new edge or arm that reaches a gated node by another way shows the weakened banner.'));
  return h('fieldset', { class: 'structure', id: 'connections' }, kids);
}

// structureForm is the node's name, its place as the entry, and its removal. A rename rewrites every
// place the name is used (the server refuses one it cannot reach everywhere); a removal lists first
// what else it removes and waits for that list to be confirmed.
function structureForm(n, v) {
  const name = h('input', { id: 'f-name', type: 'text', value: n.name, autocomplete: 'off', spellcheck: 'false', 'aria-describedby': 'f-name-help' });
  const rename = h('button', { type: 'button', class: 'btn', id: 'f-name-set' }, 'Rename');
  const doRename = () => {
    const to = name.value.trim();
    if (to === n.name) { announce('No change: that is the name already.'); return; }
    operate({ op: 'rename_node', node: n.name, value: to }, 'f-name', to);
  };
  rename.addEventListener('click', doRename);
  name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); doRename(); } });
  const order = (v.nodes || []).map((x) => x.name);
  const at = order.indexOf(n.name);
  const mvNode = (dir) => {
    const b = h('button', { type: 'button', class: 'btn', id: 'f-' + dir, 'aria-label': 'Move ' + dir + ': ' + n.name, disabled: dir === 'up' ? at <= 0 : at < 0 || at >= order.length - 1 }, dir === 'up' ? 'Move up' : 'Move down');
    b.addEventListener('click', () => {
      const to = dir === 'up' ? at - 1 : at + 1;
      const edge = dir === 'up' ? to === 0 : to === order.length - 1;
      operate({ op: 'move_node', node: n.name, value: dir }, edge ? (dir === 'up' ? 'f-down' : 'f-up') : 'f-' + dir);
    });
    return b;
  };
  const isEntry = v.entry === n.name;
  const entry = h('button', { type: 'button', class: 'btn', id: 'f-entry' }, isEntry ? 'Write the entry into the file' : 'Make this the entry node');
  entry.addEventListener('click', () => operate({ op: 'set_entry', value: n.name }, 'f-entry'));
  const remove = h('button', { type: 'button', class: 'btn', id: 'f-remove' }, 'Remove node');
  remove.addEventListener('click', () => removeNode(n, remove));
  return h('fieldset', { class: 'structure' },
    h('legend', {}, 'Node'),
    h('div', { class: 'field' }, h('label', { for: 'f-name' }, 'Name'), h('div', { class: 'inline' }, name, rename),
      h('p', { class: 'muted small', id: 'f-name-help' }, 'Renames the node everywhere the file uses the name, in wiring and assertions. The gate check follows names, so a renamed gated node reads as removed and added: the weakened banner may appear for a rename.')),
    h('div', { class: 'field' }, h('p', { class: 'small', id: 'f-pos' }, 'Position ' + (at + 1) + ' of ' + order.length + '.'),
      h('div', { class: 'inline' }, mvNode('up'), mvNode('down')),
      h('p', { class: 'muted small' }, 'Order is part of the flow (the topology digest commits to it). With no entry key the first node is the entry, so a move into or out of the first place is refused until the entry is written below.')),
    h('div', { class: 'field' }, h('p', { class: 'small' }, isEntry ? 'This is the entry node.' : 'The entry node is ' + (v.entry || 'not set') + '.'),
      h('p', { class: 'muted small' }, 'A config with no entry key starts at its first node. Adding, moving or removing nodes never changes that silently: write the entry here first.'),
      entry),
    h('div', { class: 'field' }, remove,
      h('p', { class: 'muted small' }, 'Lists what else goes with it (edges, arms, comments) before anything is removed.')));
}

async function removeNode(n, opener, opName = 'remove_node') {
  const box = $('op-error');
  let dry;
  try {
    dry = await applyOp(state.text, { op: opName, node: n.name, dry_run: true });
  } catch (err) {
    if (box) box.textContent = shown('Not applied: ' + err.message);
    announce('Not applied: ' + err.message);
    return;
  }
  const list = dry.cascade || [];
  const op = { op: opName, node: n.name, confirm: list };
  if (!list.length) { operate({ op: opName, node: n.name }, 'an-name', ''); return; }
  const go = h('button', { type: 'button', class: 'btn primary', id: 'do-remove' }, 'Remove ' + shown(n.name));
  const cancel = h('button', { type: 'button', class: 'btn' }, 'Cancel');
  cancel.addEventListener('click', () => closeDialog(opener));
  go.addEventListener('click', () => { closeDialog(); operate(op, 'an-name', ''); });
  openDialog('Remove ' + shown(n.name) + '?', [
    h('p', {}, 'Removing it also changes:'),
    h('ul', { id: 'remove-list' }, list.map((x) => h('li', {}, shown(x)))),
    h('p', { class: 'muted small' }, 'Nothing is written to the file until you save, and Undo brings it back.'),
  ], [go, cancel], cancel, opener);
}

// The add-node controls are in the page: this wires them.
function wireAddNode() {
  const name = $('an-name');
  const block = $('an-block');
  const add = async () => {
    const to = name.value.trim();
    if (await operate({ op: 'add_node', node: to, value: block.value.trim() }, 'f-name', to)) { name.value = ''; block.value = ''; }
  };
  $('an-add').addEventListener('click', add);
  for (const el of [name, block]) el.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); add(); } });
}

// gateForm is the approval section: a draft of need, approvers, separation of duties and deadline.
function gateForm(n) {
  const cur = n.approval;
  const draft = cur ? { need: cur.need, approvers: [...(cur.approvers || [])], exclude_principal: !!cur.exclude_principal, timeout: cur.timeout || '' }
    : { need: 1, approvers: [], exclude_principal: false, timeout: '' };
  const need = h('input', { id: 'g-need', type: 'number', min: '1', step: '1', value: String(draft.need), 'aria-describedby': 'g-feedback' });
  const list = h('ul', { class: 'approvers', id: 'g-approvers', 'aria-label': 'Approvers, in policy order' });
  const addId = h('input', { id: 'g-add', type: 'text', autocomplete: 'off', spellcheck: 'false', 'aria-describedby': 'g-feedback' });
  const add = h('button', { type: 'button', class: 'btn', id: 'g-add-btn' }, 'Add approver');
  const exclude = h('input', { id: 'g-exclude', type: 'checkbox', checked: draft.exclude_principal });
  const timeout = h('input', { id: 'g-timeout', type: 'text', value: draft.timeout, autocomplete: 'off', spellcheck: 'false', placeholder: 'none', 'aria-describedby': 'g-timeout-help g-feedback' });
  const feedback = h('div', { id: 'g-feedback', class: 'small', role: 'status', 'aria-live': 'polite' });
  const kofn = h('p', { class: 'small', id: 'g-kofn' });
  const apply = h('button', { type: 'button', class: 'btn primary', id: 'g-apply' }, cur ? 'Apply gate' : 'Add gate');
  const clear = cur ? h('button', { type: 'button', class: 'btn', id: 'g-clear' }, 'Remove gate') : null;

  // The feedback region is a live region: it is rewritten only when its words change, so a draft that
  // is still fine does not say "The check accepts this gate." again at every keystroke.
  let lastFeedback = '';
  function setFeedback(cls, messages) {
    const key = cls + '|' + messages.join('\n');
    if (key === lastFeedback) return;
    lastFeedback = key;
    feedback.className = 'small ' + cls;
    feedback.replaceChildren(...messages.map((m) => h('p', {}, m)));
  }
  let needBad = false;
  const operation = () => ({ op: 'set_approval', node: n.name, approval: draft });
  const timer = pause();
  let seq = 0;
  function drawApprovers() {
    list.replaceChildren(...draft.approvers.map((id, i) => {
      const rm = h('button', { type: 'button', class: 'btn', id: 'g-rm-' + i, 'aria-label': 'Remove approver ' + id }, 'Remove');
      rm.addEventListener('click', () => { draft.approvers.splice(i, 1); changed(); drawApprovers(); (list.querySelector('button') || addId).focus(); });
      return h('li', {}, h('code', {}, id), ' ', rm);
    }));
  }
  // Every change to the draft asks the server what the check says of the node with it.
  function changed() {
    apply.disabled = needBad;
    if (needBad) {
      timer.cancel();
      ++seq;
      kofn.textContent = '';
      setFeedback('bad', ['Decisions needed must be a whole number such as 2.']);
      return;
    }
    kofn.textContent = draft.approvers.length ? draft.need + ' of ' + draft.approvers.length + ' approver' + (draft.approvers.length === 1 ? '' : 's') + ' must sign' + (draft.exclude_principal ? ', never counting the person who started the run' : '') + (draft.timeout ? '; the gate expires after ' + shown(draft.timeout) : '') + '.' : 'No approvers yet.';
    timer.cancel();
    const mine = ++seq;
    timer.set(async () => {
      try {
        const e = await applyOp(state.text, operation());
        if (mine !== seq) return;
        const mineNode = (e.nodes || []).find((x) => x.name === n.name);
        const errs = ((mineNode && mineNode.problems) || []).filter((p) => p.startsWith('error'));
        setFeedback(errs.length ? 'bad' : 'good', errs.length ? errs : ['The check accepts this gate.']);
      } catch (err) {
        if (mine === seq) setFeedback('bad', [err.message]);
      }
    }, 200);
  }
  // The number is read strictly: what the field shows is what is sent. "1e3", "2.5" or "" is not a
  // whole number, and is said so instead of being sent as something else.
  need.addEventListener('input', () => {
    const raw = need.value.trim();
    needBad = !/^\d+$/.test(raw);
    draft.need = needBad ? 0 : Number(raw);
    need.setAttribute('aria-invalid', needBad ? 'true' : 'false');
    changed();
  });
  exclude.addEventListener('change', () => { draft.exclude_principal = exclude.checked; changed(); });
  timeout.addEventListener('input', () => { draft.timeout = timeout.value.trim(); changed(); });
  const addOne = () => {
    const id = addId.value.trim();
    if (!id) return;
    draft.approvers.push(id);
    addId.value = '';
    drawApprovers();
    changed();
    addId.focus();
  };
  add.addEventListener('click', addOne);
  addId.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addOne(); } });
  apply.addEventListener('click', () => operate(operation(), 'g-apply'));
  if (clear) clear.addEventListener('click', () => operate({ op: 'clear_approval', node: n.name }, 'f-safety'));
  drawApprovers();
  changed();
  return h('fieldset', { class: 'gate' },
    h('legend', {}, 'Approval gate'),
    h('p', { class: 'muted small' }, 'A person or people must sign before this node runs: ' + (cur ? 'edit the gate below.' : 'this node has no gate in the config.') + ' Approvers\' keys are supplied when a program loads the flow; the editor cannot check that they are enrolled.'),
    field('g-need', 'Decisions needed', need),
    h('div', { class: 'field' }, h('label', { for: 'g-add' }, 'Approvers'), list, h('div', { class: 'inline' }, addId, add)),
    kofn,
    h('div', { class: 'field check' }, exclude, h('label', { for: 'g-exclude' }, 'Separation of duties: never count the person who started the run')),
    field('g-timeout', 'Deadline', timeout, 'A Go duration such as 48h or 90m; empty for none. Past it the gate expires and the node never runs.'),
    feedback,
    h('div', { class: 'inline' }, apply, clear));
}

// operate applies an operation to the text, as a step the author can undo, and puts the focus back
// on the control that made it.
async function operate(op, focusId, selectAfter) {
  const sent = state.text;
  let applied = false;
  const errBox = () => $(op.op === 'add_node' ? 'an-error' : op.op === 'add_join' ? 'aj-error' : 'op-error');
  try {
    const e = await applyOp(sent, op);
    // The author may have typed while the answer was on its way: their text is not overwritten.
    if (state.text !== sent) {
      const msg = 'Not applied: the text changed while the edit was being applied. Apply it again.';
      const box = errBox();
      if (box) box.textContent = shown(msg);
      announce(msg);
      return;
    }
    if (errBox()) errBox().textContent = '';
    if (e.changed) {
      checkpoint();
      if (selectAfter !== undefined) selected = selectAfter;
      setText(e.text, e);
      announce(opWords(op));
      applied = true;
    } else {
      announce('No change: the text already says that.');
    }
    const target = focusId ? $(focusId) : null;
    if (target) target.focus();
  } catch (err) {
    const box = errBox();
    if (box) box.textContent = shown('Not applied: ' + err.message);
    announce('Not applied: ' + err.message);
  }
  return applied;
}

function opWords(op) {
  switch (op.op) {
    case 'set_safety': return op.node + ': safety ' + (op.value || 'left to the block');
    case 'set_block': return op.node + ': block ' + op.value;
    case 'clear_approval': return op.node + ': gate removed';
    case 'add_node': return 'Node ' + op.node + ' added';
    case 'remove_node': return 'Node ' + op.node + ' removed';
    case 'rename_node': return op.node + ' renamed to ' + op.value;
    case 'set_entry': return 'Entry node: ' + op.value;
    case 'move_node': return op.node + ' moved ' + op.value;
    case 'add_edge': return 'Edge added: ' + op.node + ' to ' + op.to;
    case 'remove_edge': return 'Edge removed: ' + op.node + ' to ' + op.to;
    case 'add_switch': return op.node + ' is now a switch';
    case 'remove_switch': return 'Switch on ' + op.node + ' removed';
    case 'add_arm': return 'Arm added to the switch on ' + op.node;
    case 'remove_arm': return 'Arm ' + (op.index + 1) + ' removed from the switch on ' + op.node;
    case 'set_arm': return 'Arm ' + (op.index + 1) + ' of the switch on ' + op.node + ' set';
    case 'move_arm': return 'Arm ' + (op.index + 1) + ' of the switch on ' + op.node + ' moved ' + op.value;
    case 'add_join': return 'Join ' + op.node + ' added';
    case 'remove_join': return 'Join ' + op.node + ' removed';
    case 'set_join_inputs': return 'Inputs of ' + op.node + ' set';
    case 'set_join_merge': return 'Merge of ' + op.node + ' set';
    case 'set_loop_max': return op.max ? 'Arm ' + (op.index + 1) + ' loops at most ' + op.max : 'Arm ' + (op.index + 1) + ' is an ordinary arm';
    case 'set_else': return op.to ? 'Else of the switch on ' + op.node + ' set' : 'Else of the switch on ' + op.node + ' removed';
    case 'move_wiring': return 'Wiring element ' + (op.index + 1) + ' moved ' + op.value;
    case 'set_flow': return 'Flow name set';
    case 'set_in': return op.value ? 'Input type set' : 'Input type removed';
    case 'set_out': return op.value ? 'Output type set' : 'Output type removed';
    case 'add_assertion': return 'Assertion added';
    case 'set_assertion': return 'Assertion changed';
    case 'remove_assertion': return 'Assertion removed';
    default: return op.node + ': gate set';
  }
}

// ---- undo and redo: a stack of earlier texts ----

const undoStack = [];
const redoStack = [];
function checkpoint() {
  if (undoStack.at(-1) !== state.text) pushCapped(undoStack, state.text);
  redoStack.length = 0;
  renderState();
}
// The undo and redo stacks hold whole texts (at most a megabyte each), so they are capped: the oldest
// step is dropped past this many.
const MAX_HISTORY = 100;
const pushCapped = (stack, text) => { stack.push(text); if (stack.length > MAX_HISTORY) stack.shift(); };
let stepping = false;
async function step(from, to, words) {
  if (!from.length || stepping) return;
  stepping = true;
  const sent = state.text;
  const text = from.at(-1);
  try {
    // Ask for the check first: the stacks move only when the step is going to happen.
    const v = await api('/api/report', { text });
    if (state.text !== sent) {
      announce('Not ' + words.toLowerCase() + ': the text changed while the step was being made.');
      return;
    }
    from.pop();
    pushCapped(to, sent);
    setText(text, v);
    announce(words);
  } catch (err) {
    announce('Not ' + words.toLowerCase() + ': ' + err.message);
  } finally {
    stepping = false;
    renderState();
  }
}
const undo = () => step(undoStack, redoStack, 'Undone');
const redo = () => step(redoStack, undoStack, 'Redone');

// setText makes text the page's text: the source, the check, and every pane.
function setText(text, v) {
  state.text = text;
  $('source').value = text;
  render(v, { keepSource: true });
}

// ---- the assertions panel: the six forms as templates, each with its live verdict ----
//
// An assertion is written from a form, not typed: the page sends the form's parameters
// ({form, node, via, k, n, excluding}) and the server writes the line the check reads. While the form
// is filled in, each change asks the server what the check says of the line (the apply call, not
// kept) and shows the verdict, so the author sees whether it holds, or its counterexample, before
// adding it. Assertions are checked by bide check and in CI; plan.Load ignores them.
//   v.assertion_forms[i] is assert line i read as a form, null when it is none of the six.

const TEMPLATES = [
  ['approval', 'Approval: every path to a node passes a gate of at least K decisions'],
  ['passes', 'Passes: every path to a node passes another node'],
  ['terminal', 'Terminal: every path to a terminal passes a node'],
  ['no_side_effect', 'No side effect: nothing with a side effect comes after a node'],
  ['reachable', 'Reachable: a node can be reached'],
  ['unreachable', 'Unreachable: a node cannot be reached'],
];

let assertEditing = -1; // the index of the assert line being edited, -1 for the add form

function verdictPill(a) {
  const state = a.holds ? 'holds' : a.invalid ? 'invalid' : a.undecided ? 'undecided' : 'violated';
  const glyph = a.holds ? 'ok' : a.invalid || a.undecided ? 'unknown' : 'x';
  const cls = a.holds ? 'state-completed' : a.invalid || a.undecided ? 'state-started' : 'state-halted';
  return h('span', { class: 'pill ' + cls }, icon(glyph), ' ' + state);
}

function verdictDetail(a) {
  if (a.holds) return null;
  return h('p', { class: 'small ' + (a.invalid || a.undecided ? 'warn' : 'bad') }, a.message || '', (a.path || []).length ? ' counterexample: ' + a.path.join(' \u2192 ') : '');
}

export function renderAssertions(v) {
  const list = v.assertions || [];
  const forms = v.assertion_forms || [];
  const rows = list.map((a, i) => {
    const edit = forms[i] ? h('button', { type: 'button', class: 'btn', id: 'a-edit-' + i, 'aria-label': 'Edit assertion ' + a.text }, 'Edit') : null;
    if (edit) edit.addEventListener('click', () => { assertEditing = i; renderAssertions(view); $('a-template').focus(); });
    const rm = h('button', { type: 'button', class: 'btn', id: 'a-rm-' + i, 'aria-label': 'Remove assertion ' + a.text }, 'Remove');
    rm.addEventListener('click', () => { assertEditing = -1; operate({ op: 'remove_assertion', index: i }, 'a-template'); });
    return h('div', { class: 'assert', id: 'a-row-' + i },
      h('p', {}, verdictPill(a), ' ', h('code', {}, a.text), ' ', edit, rm), verdictDetail(a));
  });
  if (assertEditing >= list.length || (assertEditing >= 0 && !forms[assertEditing])) assertEditing = -1;
  replace('assertions', list.length ? rows : h('p', { class: 'muted' }, 'None declared.'), assertionForm(v, assertEditing));
}

function nodeSelect(id, label, v, value, onChange) {
  const sel = h('select', { id }, (v.nodes || []).map((n) => h('option', { value: n.name, selected: n.name === value }, shown(n.name))));
  sel.addEventListener('change', onChange);
  return field(id, label, sel);
}

function assertionForm(v, editing) {
  const names = (v.nodes || []).map((n) => n.name);
  const forms = v.assertion_forms || [];
  const cur = editing >= 0 ? forms[editing] : null;
  const d = cur ? { ...cur } : { form: 'approval', node: names[0] || '', via: names[0] || '', k: 1, n: 0, excluding: false };
  if (!d.node) d.node = names[0] || '';
  if (!d.via) d.via = names[0] || '';
  if (!d.k) d.k = 1;
  const tpl = h('select', { id: 'a-template' }, TEMPLATES.map(([val, label]) => h('option', { value: val, selected: val === d.form }, label)));
  const fields = h('div', {});
  const live = h('div', { id: 'a-live', class: 'small', role: 'status', 'aria-live': 'polite' });
  const submit = h('button', { type: 'button', class: 'btn primary', id: 'a-submit' }, editing >= 0 ? 'Save assertion' : 'Add assertion');
  const cancel = editing >= 0 ? h('button', { type: 'button', class: 'btn', id: 'a-cancel' }, 'Cancel') : null;
  const operation = () => ({ op: editing >= 0 ? 'set_assertion' : 'add_assertion', index: editing >= 0 ? editing : 0, assertion: { ...d } });
  const timer = pause();
  let seq = 0;
  function draw() {
    const kids = [];
    if (d.form === 'approval' || d.form === 'passes' || d.form === 'no_side_effect' || d.form === 'reachable' || d.form === 'unreachable') {
      kids.push(nodeSelect('a-node', 'Node', v, d.node, (e) => { d.node = e.target.value; changed(); }));
    }
    if (d.form === 'passes' || d.form === 'terminal') {
      kids.push(nodeSelect('a-via', d.form === 'terminal' ? 'Node every terminal path passes' : 'Node it must pass', v, d.via, (e) => { d.via = e.target.value; changed(); }));
    }
    if (d.form === 'approval') {
      const k = h('input', { id: 'a-k', type: 'number', min: '1', step: '1', value: String(d.k) });
      k.addEventListener('input', () => { d.k = Number.parseInt(k.value, 10) || 0; changed(); });
      const n = h('input', { id: 'a-n', type: 'number', min: '0', step: '1', value: d.n ? String(d.n) : '', placeholder: 'no limit' });
      n.addEventListener('input', () => { d.n = Number.parseInt(n.value, 10) || 0; changed(); });
      const ex = h('input', { id: 'a-excluding', type: 'checkbox', checked: !!d.excluding });
      ex.addEventListener('change', () => { d.excluding = ex.checked; changed(); });
      kids.push(field('a-k', 'Decisions needed (K)', k), field('a-n', 'At most this many approvers (N, optional)', n),
        h('div', { class: 'field check' }, ex, h('label', { for: 'a-excluding' }, 'The gate never counts the person who started the run')));
    }
    fields.replaceChildren(...kids);
  }
  function changed() {
    timer.cancel();
    const mine = ++seq;
    timer.set(async () => {
      try {
        const e = await applyOp(state.text, operation());
        if (mine !== seq) return;
        const at = editing >= 0 ? editing : (e.assertions || []).length - 1;
        const a = (e.assertions || [])[at];
        live.className = 'small';
        live.replaceChildren(a ? h('p', {}, 'Live verdict: ', verdictPill(a), ' ', h('code', {}, a.text)) : h('p', {}, 'No change.'), a ? verdictDetail(a) : null);
      } catch (err) {
        if (mine !== seq) return;
        live.className = 'small bad';
        live.replaceChildren(h('p', {}, err.message));
      }
    }, 150);
  }
  tpl.addEventListener('change', () => { d.form = tpl.value; draw(); changed(); });
  submit.addEventListener('click', async () => { const idx = assertEditing; assertEditing = -1; await operate(operation(), idx >= 0 ? 'a-edit-' + idx : 'a-template'); });
  if (cancel) cancel.addEventListener('click', () => { assertEditing = -1; renderAssertions(view); $('a-edit-' + editing)?.focus(); });
  draw();
  changed();
  return h('fieldset', { class: 'gate assertion-form' },
    h('legend', {}, editing >= 0 ? 'Edit assertion' : 'Add an assertion'),
    field('a-template', 'Form', tpl),
    fields, live, h('div', { class: 'inline' }, submit, cancel));
}

export function renderProblems(v) {
  const list = v.problems || [];
  if (list.length === 0) { replace('problems', h('p', { class: 'good' }, 'None found.')); return; }
  replace('problems', h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Where'), h('th', {}, 'Problem'))),
    h('tbody', {}, list.map((p) => h('tr', {},
      h('td', {}, h('code', {}, shown(p.node || (p.from || p.to ? (p.from || '') + ' → ' + (p.to || '') : 'config')))),
      h('td', { class: p.severity === 'error' ? 'bad' : 'warn' }, p.severity + ': ' + shown(p.message)))))));
}

export function renderCanvas(v) {
  const wrap = $('canvas');
  if (!v.graph) { wrap.replaceChildren(h('p', { class: 'muted' }, v.parse_error ? 'The config does not parse, so there is no graph.' : 'No nodes to draw.')); return; }
  const doc = new DOMParser().parseFromString(v.graph, 'image/svg+xml');
  const svg = doc.documentElement;
  if (svg.localName !== 'svg' || doc.querySelector('parsererror')) { wrap.replaceChildren(h('p', { class: 'bad' }, 'The graph could not be drawn.')); return; }
  abortDrag(); // the port being dragged is about to leave the page
  wrap.replaceChildren(document.adoptNode(svg));
  for (const t of wrap.querySelectorAll('text, title')) if (t.children.length === 0) t.textContent = shown(t.textContent);
  for (const e of wrap.querySelectorAll('[aria-label], [title]')) for (const a of ['aria-label', 'title']) if (e.hasAttribute(a)) e.setAttribute(a, shown(e.getAttribute(a)));
  for (const g of wrap.querySelectorAll('g.node')) {
    const name = g.getAttribute('data-id');
    g.setAttribute('role', 'button'); // the drawing marks a node as an image; here it can be pressed
    // The node is one button. Its name comes from the <title> the drawing gives it (the node, its
    // safety and gate, with the details), not from an aria-label that would have to repeat the words
    // drawn inside it, which a name must contain; the drawn words are the same facts, so they are
    // hidden from assistive technology rather than read twice.
    g.removeAttribute('aria-label');
    for (const t of g.querySelectorAll('text')) t.setAttribute('aria-hidden', 'true');
    g.addEventListener('click', () => select(name));
    g.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); select(name); } });
    addPort(svg, g, name);
  }
  if (typeof ResizeObserver === 'function') new ResizeObserver(() => { for (const fo of svg.querySelectorAll('foreignObject.port-hit')) fitHit(svg, fo); }).observe(svg);
  markSelected();
}

// ---- drag to connect: an extra, never the only way ----
//
// A small circle at a node's right edge can be dragged onto another node to add an edge from the first
// to the second, the same add_edge the Connections form sends (so the same refusals, the same
// verification, the same weakened banner). It is a pointer shortcut and nothing depends on it:
// every connection can be made, and removed, from the inspector by keyboard, and the circle is
// hidden from assistive technology and not in the Tab order. Escape cancels a drag in progress.
// Keep a port's hit area at least 44 CSS px square at the svg's current scale (the svg is scaled to the
// window's width, so this runs again when its size changes).
function fitHit(svg, fo) {
  const ctm = svg.getScreenCTM();
  const scale = ctm && ctm.a > 0 ? ctm.a : 1;
  const side = Math.max(16, 44 / scale);
  const x = Number(fo.getAttribute('data-cx'));
  const y = Number(fo.getAttribute('data-cy'));
  fo.setAttribute('x', String(x - side / 2));
  fo.setAttribute('y', String(y - side / 2));
  fo.setAttribute('width', String(side));
  fo.setAttribute('height', String(side));
}

// The drag in progress, if any: its cleanup, so a redraw or a second pointer can end it.
let activeDrag = null;
function abortDrag() { if (activeDrag) activeDrag(); }

function addPort(svg, g, from) {
  const box = g.querySelector('rect.box');
  if (!box) return;
  const x = Number(box.getAttribute('x')) + Number(box.getAttribute('width'));
  const y = Number(box.getAttribute('y')) + Number(box.getAttribute('height')) / 2;
  const port = document.createElementNS(SVGNS, 'circle');
  port.setAttribute('class', 'port');
  port.setAttribute('cx', String(x));
  port.setAttribute('cy', String(y));
  port.setAttribute('r', '8');
  port.setAttribute('aria-hidden', 'true');
  g.append(port);
  // The circle is only the picture. What takes the pointer is an HTML box in a foreignObject, at least
  // 44 CSS px square whatever the svg's scale: Chrome honours touch-action on HTML elements but not on
  // SVG shapes, and a touch that starts here must not be taken for a scroll while a touch anywhere
  // else on the flow still scrolls the page.
  const fo = document.createElementNS(SVGNS, 'foreignObject');
  fo.setAttribute('class', 'port-hit');
  fo.setAttribute('data-cx', String(x));
  fo.setAttribute('data-cy', String(y));
  const hit = document.createElementNS('http://www.w3.org/1999/xhtml', 'div');
  hit.setAttribute('class', 'port-hit-box');
  hit.setAttribute('aria-hidden', 'true');
  fo.append(hit);
  g.append(fo);
  fitHit(svg, fo);
  const handle = hit;
  const at = (e) => {
    const pt = svg.createSVGPoint();
    pt.x = e.clientX;
    pt.y = e.clientY;
    return pt.matrixTransform(svg.getScreenCTM().inverse());
  };
  handle.addEventListener('click', (e) => e.stopPropagation()); // a drag is not a selection
  handle.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.stopPropagation();
    abortDrag();
    handle.setPointerCapture(e.pointerId);
    select(from);
    const line = document.createElementNS(SVGNS, 'line');
    line.setAttribute('class', 'drag');
    line.setAttribute('x1', String(x));
    line.setAttribute('y1', String(y));
    line.setAttribute('x2', String(x));
    line.setAttribute('y2', String(y));
    svg.append(line);
    let cancelled = false;
    const finish = () => {
      if (activeDrag === cancel) activeDrag = null;
      line.remove();
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', cancel);
      handle.removeEventListener('lostpointercapture', cancel);
      document.removeEventListener('keydown', key, true);
    };
    const move = (m) => { const p = at(m); line.setAttribute('x2', String(p.x)); line.setAttribute('y2', String(p.y)); };
    const cancel = () => { if (cancelled) return; cancelled = true; finish(); announce('Connection cancelled'); };
    const key = (k) => { if (k.key === 'Escape') { k.preventDefault(); cancel(); } };
    const up = (u) => {
      if (cancelled) return;
      const under = document.elementFromPoint(u.clientX, u.clientY);
      const to = under && under.closest ? under.closest('g.node') : null;
      finish();
      const target = to && to.getAttribute('data-id');
      if (!target || target === from) { announce('Connection cancelled'); return; }
      operate({ op: 'add_edge', node: from, to: target }, null);
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', cancel);
    handle.addEventListener('lostpointercapture', cancel);
    activeDrag = cancel;
    document.addEventListener('keydown', key, true);
  });
}

function markSelected() {
  for (const g of $('canvas').querySelectorAll('g.node')) {
    g.classList.toggle('sel', g.getAttribute('data-id') === selected);
    g.setAttribute('aria-pressed', g.getAttribute('data-id') === selected ? 'true' : 'false');
  }
  for (const b of $('outline').querySelectorAll('button')) b.setAttribute('aria-current', b.getAttribute('data-node') === selected ? 'true' : 'false');
}

function select(name) {
  selected = name;
  markSelected();
  renderInspector(view);
  announce('Selected ' + name);
}

// What the page holds of the file: text is what the author has now, disk is the text the file had
// when it was last read or saved and hash that file's hash. text differs from disk exactly when there
// are unsaved changes.
const state = { text: '', disk: '', hash: '' };
const dirty = () => state.text !== state.disk;

export function render(v, { keepSource = false } = {}) {
  view = v;
  if (selected && !(v.nodes || []).some((n) => n.name === selected)) selected = '';
  $('file').textContent = shown(v.name + ' (' + v.format + ')');
  $('modeline').textContent = v.mode;
  if (!keepSource) $('source').value = v.text;
  renderWeakening(v);
  renderVerdict(v);
  renderCanvas(v);
  renderOutline(v);
  renderInspector(v);
  renderAssertions(v);
  renderProblems(v);
  renderState();
  // The weakened banner announces itself (it is an alert); the verdict is spoken here, once per change.
  const verdict = v.verdict.word + '. ' + v.verdict.line;
  if (verdict !== lastVerdict) { lastVerdict = verdict; announce(verdict); }
}

function renderState() {
  const d = dirty();
  $('state-tag').textContent = d ? 'unsaved changes' : 'saved';
  $('save').disabled = !d;
  $('revert').disabled = !d;
  $('undo').disabled = undoStack.length === 0;
  $('redo').disabled = redoStack.length === 0;
}

// Typing in the source asks the server for the check of the new text (the page validates nothing
// itself) after a short pause. A slower answer to an older text is dropped.
let typing = 0;
const timer = pause();
let lastTyped = 0;
function onInput() {
  const now = Date.now();
  if (now - lastTyped > 1000) { pushCapped(undoStack, state.text); redoStack.length = 0; } // a burst of typing is one undo step
  lastTyped = now;
  state.text = $('source').value;
  renderState();
  timer.cancel();
  const mine = ++typing;
  timer.set(async () => {
    try {
      const v = await api('/api/report', { text: state.text });
      if (mine === typing) render(v, { keepSource: true });
    } catch (e) {
      announce('The check failed: ' + e.message);
    }
  }, 250);
}

async function revert() {
  checkpoint();
  state.text = state.disk;
  $('source').value = state.text;
  render(await api('/api/report', { text: state.text }));
  announce('Reverted to the saved text');
}

// ---- the save dialog: what will be written, and what the author must confirm ----

const dlg = () => $('dialog');
const baseName = (p) => p.split(/[\\/]/).pop();
function closeDialog(returnFocusTo) {
  if (dlg().open) dlg().close();
  if (returnFocusTo) returnFocusTo.focus();
}

// diffBlock draws a unified diff as lines of text; the leading + or - is part of the text, so
// added and removed lines are not told apart by color alone.
function diffBlock(diff) {
  return h('pre', { class: 'diff', tabindex: '0', 'aria-label': 'Diff of the bytes to be written' },
    diff.split('\n').filter((l, i, a) => !(i === a.length - 1 && l === '')).map((l) =>
      h('span', { class: l.startsWith('+++') || l.startsWith('---') || l.startsWith('@@') ? 'dh' : l[0] === '+' ? 'da' : l[0] === '-' ? 'dd' : 'dc' }, l || ' ')));
}

// The dialog gives the focus back to the control that opened it however it closes (Escape included).
let dialogOpener = null;
function openDialog(title, body, actions, labelFor, opener) {
  if (opener) dialogOpener = opener;
  $('dialog-title').textContent = shown(title);
  replace('dialog-body', body);
  replace('dialog-actions', actions);
  if (!dlg().open) dlg().showModal();
  (labelFor || dlg().querySelector('button')).focus();
}

async function save() {
  const opener = $('save');
  let pv;
  try {
    pv = await api('/api/preview', { text: state.text, hash: state.hash });
  } catch (e) {
    announce('Could not prepare the save: ' + e.message);
    return;
  }
  if (pv.conflict) return conflictDialog(pv.hash, pv.diff, opener);
  pv.needs_confirmation = pv.needs_confirmation || [];
  const cancel = h('button', { type: 'button', class: 'btn' }, 'Cancel');
  cancel.addEventListener('click', () => closeDialog(opener));
  const confirmBox = h('input', { type: 'checkbox', id: 'confirm-weaken' });
  const go = h('button', { type: 'button', class: 'btn primary', id: 'do-save', disabled: pv.needs_confirmation.length > 0 }, 'Save');
  confirmBox.addEventListener('change', () => { go.disabled = !confirmBox.checked; });
  const status = h('p', { class: 'bad', role: 'alert', id: 'save-error' });
  go.addEventListener('click', () => commit(pv, opener, status));
  const body = [
    !pv.changed ? h('p', {}, 'The text is the same as the file: nothing to write.') : null,
    pv.errors > 0 ? h('p', { class: 'warn' }, 'The text has ' + pv.errors + ' error' + (pv.errors === 1 ? '' : 's') + ' (bide check lists them). It can be saved, and it will fail bide check and plan.Load until they are fixed.') : null,
    (pv.removed_comments || []).length ? h('div', {}, h('p', { class: 'warn' }, 'This save removes ' + pv.removed_comments.length + ' comment' + (pv.removed_comments.length === 1 ? '' : 's') + ':'),
      h('ul', {}, pv.removed_comments.map((c) => h('li', {}, h('code', {}, c))))) : null,
    pv.needs_confirmation.length ? h('div', { class: 'verdict v-bad' }, h('div', { class: 'vicon' }, icon('halt')), h('div', { class: 'vtext' },
      h('p', { class: 'vword' }, 'THIS WEAKENS THE POLICY'),
      h('p', { class: 'vline' }, 'Compared with ' + (view.weakening ? view.weakening.baseline : 'the baseline') + ', saving this:'),
      pv.needs_confirmation.map((x) => h('p', { class: 'vdetail' }, x)),
      h('p', { class: 'vdetail' }, h('label', {}, confirmBox, ' I confirm that I am weakening this policy.')))) : null,
    pv.changed ? diffBlock(pv.diff) : null,
    status,
  ];
  openDialog('Save ' + baseName(view.name) + '?', body, [go, cancel], pv.needs_confirmation.length ? confirmBox : go, opener);
}

async function commit(pv, opener, status) {
  status.textContent = '';
  try {
    const d = await api('/api/save', { text: state.text, hash: state.hash, confirmed: pv.needs_confirmation });
    state.disk = d.text;
    state.hash = d.hash;
    closeDialog();
    render(d, { keepSource: true });
    $('source').focus(); // the Save button is disabled now that nothing is unsaved
    announce('Saved ' + baseName(d.name));
  } catch (e) {
    if (e.status === 409) { conflictDialog(e.data.hash, e.data.diff, opener); return; }
    if (e.status === 422) { closeDialog(); save(); return; } // the text changed under the confirmation: ask again
    status.textContent = shown('Not saved: ' + e.message);
  }
}

// The file changed on disk since the page read it. The editor never overwrites it: it shows what is
// on disk against the author's text and offers to reload (which replaces the author's edits) or to
// go on editing.
function conflictDialog(hash, diff, opener) {
  const keep = h('button', { type: 'button', class: 'btn' }, 'Keep editing');
  keep.addEventListener('click', () => closeDialog(opener));
  const reload = h('button', { type: 'button', class: 'btn primary' }, 'Reload from disk (discards my edits)');
  reload.addEventListener('click', async () => {
    try {
      const d = await api('/api/doc');
      state.text = state.disk = d.text;
      state.hash = d.hash;
      closeDialog(opener);
      render(d);
      announce('Reloaded from disk');
    } catch (e) {
      announce('Could not reload: ' + e.message);
    }
  });
  openDialog('The file changed on disk', [
    h('p', { class: 'bad', role: 'alert' }, 'The file was changed by something else after this page read it, so nothing was written. The diff below is the file on disk (lines starting with -) against your text (+).'),
    diffBlock(diff || ''),
  ], [keep, reload], keep, opener);
}

function theme() {
  const root = document.documentElement;
  const now = root.getAttribute('data-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  const next = now === 'dark' ? 'light' : 'dark';
  root.setAttribute('data-theme', next);
  try { localStorage.setItem('bide-theme', next); } catch (e) { /* storage is blocked: the choice lasts for this page */ }
}

async function main() {
  $('theme').addEventListener('click', theme);
  if (!token) {
    replace('verdict', h('p', { class: 'bad' }, 'No session token in the address. Open the address bide edit printed, including the part after #.'));
    return;
  }
  $('source').addEventListener('input', onInput);
  $('outline').addEventListener('keydown', outlineKeys);
  $('wiring').addEventListener('keydown', outlineKeys);
  wireAddNode();
  wireSkipLinks();
  dlg().addEventListener('close', () => {
    if (dlg().open) return; // closed to be opened again (a save that must be asked again)
    const o = dialogOpener;
    dialogOpener = null;
    if (o && !o.disabled) o.focus();
    else if (o) $('source').focus();
  });
  $('save').addEventListener('click', save);
  $('revert').addEventListener('click', revert);
  $('undo').addEventListener('click', undo);
  $('redo').addEventListener('click', redo);
  addEventListener('beforeunload', (e) => { if (dirty()) { e.preventDefault(); e.returnValue = ''; } });
  try {
    const d = await api('/api/doc');
    state.text = state.disk = d.text;
    state.hash = d.hash;
    render(d);
  } catch (e) {
    replace('verdict', h('p', { class: 'bad' }, 'Could not read the file: ' + e.message));
  }
}

if (typeof document !== 'undefined' && document.getElementById('verdict')) main();
