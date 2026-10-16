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

// h builds an element: h('p', {class: 'x'}, 'text', child).
export function h(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    e.setAttribute(k, v === true ? '' : String(v));
  }
  for (const k of kids.flat()) {
    if (k == null || k === false) continue;
    e.append(typeof k === 'string' ? document.createTextNode(k) : k);
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

async function api(path, body = {}) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Bide-Token': token },
    body: JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(data.error || res.status + ' ' + res.statusText), { status: res.status, data });
  return data;
}

const $ = (id) => document.getElementById(id);
const replace = (id, ...kids) => $(id).replaceChildren(...kids.flat().filter(Boolean));
const announce = (msg) => { $('live').textContent = msg; };

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
export function renderWeakening(v) {
  const w = v.weakening;
  if (!w) { replace('weakened'); replace('baseline'); return; }
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
  const words = [];
  if (!(w.weakened && w.weakened.length) && !w.compare_error) words.push('not weaker than');
  else words.push('compared with');
  const ch = (w.changes || []).length;
  replace('baseline', 'Baseline: ' + words[0] + ' ' + w.baseline + (ch ? ' (' + ch + ' change' + (ch === 1 ? '' : 's') + ')' : ' (no changes)') +
    ((w.strengthened || []).length ? '; stronger in ' + w.strengthened.length + ' way' + (w.strengthened.length === 1 ? '' : 's') : '') + '.');
}

export function renderOutline(v) {
  replace('outline', (v.nodes || []).map((n) => {
    const b = h('button', { type: 'button', 'data-node': n.name, 'aria-current': n.name === selected ? 'true' : 'false' },
      h('span', { class: 'nm' }, n.name),
      h('span', { class: 'meta' }, [n.kind, n.block, n.risk, n.gate ? 'gate ' + n.gate : ''].filter(Boolean).join(' · ')),
      (n.problems || []).map((p) => h('span', { class: 'prob' }, p)));
    b.addEventListener('click', () => select(n.name));
    return h('li', {}, b);
  }));
  replace('wiring', (v.wiring || []).map((w) => h('li', {}, w)));
}

// ---- the inspector: a node's block, safety and approval gate, as forms ----
//
// A form change is an operation sent to the server (POST /api/apply {text, operation}); the answer
// is the new text and its check, which the page shows. The gate form edits a draft: each change to
// it asks the server what the check would say of the node with that gate (the same call, not kept),
// and shows the node's problems under the form, so what the form rejects is what bide check
// rejects. Apply writes the draft into the text.
//   POST /api/apply {text, operation{op, node, value, approval{need, approvers[], exclude_principal, timeout}}}
//     -> View + { changed }, or 422 { error } when the operation cannot be applied

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
  const row = (k, val) => val ? [h('dt', {}, k), h('dd', {}, val)] : [];
  const blocks = [...new Set((v.nodes || []).map((x) => x.block).filter(Boolean))];
  const safety = h('select', { id: 'f-safety' }, SAFETIES.map(([val, label]) => h('option', { value: val, selected: val === (n.safety || '') }, label)));
  safety.addEventListener('change', () => operate({ op: 'set_safety', node: n.name, value: safety.value }, 'f-safety'));
  const block = h('input', { id: 'f-block', type: 'text', value: n.block || '', list: 'f-blocks', autocomplete: 'off', spellcheck: 'false' });
  const setBlock = h('button', { type: 'button', class: 'btn', id: 'f-block-set' }, 'Set block');
  setBlock.addEventListener('click', () => operate({ op: 'set_block', node: n.name, value: block.value.trim() }, 'f-block'));
  block.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); setBlock.click(); } });
  replace('inspector',
    h('h3', {}, n.name),
    h('dl', {}, row('kind', n.kind), row('crash behavior', n.risk), row('approval gate', n.gate || 'none'), row('problems', (n.problems || []).join('; '))),
    h('p', { id: 'op-error', class: 'bad small', role: 'alert' }),
    h('div', { class: 'form' },
      field('f-safety', 'Safety', safety, 'Only lowers what the block declares in Go; the check cannot see that here, so a value above it fails when a program loads the flow.'),
      h('div', { class: 'field' }, h('label', { for: 'f-block' }, 'Block'), h('div', { class: 'inline' }, block, setBlock),
        h('datalist', { id: 'f-blocks' }, blocks.map((b) => h('option', { value: b }))),
        h('p', { class: 'muted small' }, 'A block name is chosen, not written: its Go code is registered in your program.')),
      gateForm(n)));
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

  const operation = () => ({ op: 'set_approval', node: n.name, approval: draft });
  let timer = 0;
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
    kofn.textContent = draft.approvers.length ? draft.need + ' of ' + draft.approvers.length + ' approvers must sign' + (draft.exclude_principal ? ', never counting the person who started the run' : '') + (draft.timeout ? '; the gate expires after ' + draft.timeout : '') + '.' : 'No approvers yet.';
    clearTimeout(timer);
    const mine = ++seq;
    timer = setTimeout(async () => {
      try {
        const e = await api('/api/apply', { text: state.text, operation: operation() });
        if (mine !== seq) return;
        const mineNode = (e.nodes || []).find((x) => x.name === n.name);
        const errs = ((mineNode && mineNode.problems) || []).filter((p) => p.startsWith('error'));
        feedback.className = 'small ' + (errs.length ? 'bad' : 'good');
        feedback.replaceChildren(...(errs.length ? errs.map((p) => h('p', {}, p)) : [h('p', {}, 'The check accepts this gate.')]));
      } catch (err) {
        if (mine === seq) { feedback.className = 'small bad'; feedback.replaceChildren(h('p', {}, err.message)); }
      }
    }, 200);
  }
  need.addEventListener('input', () => { draft.need = Number.parseInt(need.value, 10) || 0; changed(); });
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
async function operate(op, focusId) {
  try {
    const e = await api('/api/apply', { text: state.text, operation: op });
    if (e.changed) {
      checkpoint();
      setText(e.text, e);
      announce(opWords(op));
    } else {
      announce('No change: the text already says that.');
    }
    const target = $(focusId);
    if (target) target.focus();
  } catch (err) {
    const box = $('op-error');
    if (box) box.textContent = 'Not applied: ' + err.message;
    announce('Not applied: ' + err.message);
  }
}

function opWords(op) {
  switch (op.op) {
    case 'set_safety': return op.node + ': safety ' + (op.value || 'left to the block');
    case 'set_block': return op.node + ': block ' + op.value;
    case 'clear_approval': return op.node + ': gate removed';
    default: return op.node + ': gate set';
  }
}

// ---- undo and redo: a stack of earlier texts ----

const undoStack = [];
const redoStack = [];
function checkpoint() {
  if (undoStack.at(-1) !== state.text) undoStack.push(state.text);
  redoStack.length = 0;
  renderState();
}
async function step(from, to, words) {
  if (!from.length) return;
  to.push(state.text);
  const text = from.pop();
  setText(text, await api('/api/report', { text }));
  announce(words);
}
const undo = () => step(undoStack, redoStack, 'Undone');
const redo = () => step(redoStack, undoStack, 'Redone');

// setText makes text the page's text: the source, the check, and every pane.
function setText(text, v) {
  state.text = text;
  $('source').value = text;
  render(v, { keepSource: true });
}

export function renderAssertions(v) {
  const list = v.assertions || [];
  if (list.length === 0) { replace('assertions', h('p', { class: 'muted' }, 'None declared.')); return; }
  replace('assertions', list.map((a) => {
    const state = a.holds ? 'holds' : a.invalid ? 'invalid' : a.undecided ? 'undecided' : 'violated';
    const glyph = a.holds ? 'ok' : a.invalid || a.undecided ? 'unknown' : 'x';
    const cls = a.holds ? 'state-completed' : a.invalid || a.undecided ? 'state-started' : 'state-halted';
    return h('div', { class: 'assert' },
      h('p', {}, h('span', { class: 'pill ' + cls }, icon(glyph), ' ' + state), ' ', h('code', {}, a.text)),
      a.holds ? null : h('p', { class: 'small ' + (a.invalid || a.undecided ? 'warn' : 'bad') }, a.message || '',
        (a.path || []).length ? ' counterexample: ' + a.path.join(' → ') : ''));
  }));
}

export function renderProblems(v) {
  const list = v.problems || [];
  if (list.length === 0) { replace('problems', h('p', { class: 'good' }, 'None found.')); return; }
  replace('problems', h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Where'), h('th', {}, 'Problem'))),
    h('tbody', {}, list.map((p) => h('tr', {},
      h('td', {}, h('code', {}, p.node || (p.from || p.to ? (p.from || '') + ' → ' + (p.to || '') : 'config'))),
      h('td', { class: p.severity === 'error' ? 'bad' : 'warn' }, p.severity + ': ' + p.message))))));
}

export function renderCanvas(v) {
  const wrap = $('canvas');
  if (!v.graph) { wrap.replaceChildren(h('p', { class: 'muted' }, v.parse_error ? 'The config does not parse, so there is no graph.' : 'No nodes to draw.')); return; }
  const doc = new DOMParser().parseFromString(v.graph, 'image/svg+xml');
  const svg = doc.documentElement;
  if (svg.localName !== 'svg' || doc.querySelector('parsererror')) { wrap.replaceChildren(h('p', { class: 'bad' }, 'The graph could not be drawn.')); return; }
  wrap.replaceChildren(document.adoptNode(svg));
  for (const g of wrap.querySelectorAll('g.node')) {
    const name = g.getAttribute('data-id');
    g.addEventListener('click', () => select(name));
    g.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); select(name); } });
  }
  markSelected();
}

function markSelected() {
  for (const g of $('canvas').querySelectorAll('g.node')) g.classList.toggle('sel', g.getAttribute('data-id') === selected);
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
  $('file').textContent = v.name + ' (' + v.format + ')';
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
  announce((v.weakening && v.weakening.weakened && v.weakening.weakened.length ? (v.weakening.word || 'WEAKENED') + '. ' : '') + v.verdict.word + '. ' + v.verdict.line);
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
let timer = 0;
let lastTyped = 0;
function onInput() {
  const now = Date.now();
  if (now - lastTyped > 1000) { undoStack.push(state.text); redoStack.length = 0; } // a burst of typing is one undo step
  lastTyped = now;
  state.text = $('source').value;
  renderState();
  clearTimeout(timer);
  const mine = ++typing;
  timer = setTimeout(async () => {
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

function openDialog(title, body, actions, labelFor) {
  $('dialog-title').textContent = title;
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
  openDialog('Save ' + baseName(view.name) + '?', body, [go, cancel], pv.needs_confirmation.length ? confirmBox : go);
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
    status.textContent = 'Not saved: ' + e.message;
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
  ], [keep, reload], keep);
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
