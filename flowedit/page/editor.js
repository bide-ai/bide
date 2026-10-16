// bide edit: the page. It holds the config text and draws what the server computes; it validates
// nothing itself. Config text is untrusted: every name, comment and message goes into the page with
// textContent or a property, never as markup, and the graph (SVG the server escapes) is parsed with
// DOMParser and adopted.
//
// Request and response shapes (the Go side: flowedit.Doc and flowedit.View):
//   POST /api/doc    {}             -> Doc  { ...View, hash }
//   POST /api/report {text}         -> View { name, format, text, mode, parse_error, flow, entry,
//                                             verdict{class,word,icon,line,details,stats[]}, errors, warnings,
//                                             problems[], assertions[], nodes[], wiring[], graph }
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
  if (!res.ok) throw new Error(data.error || res.status + ' ' + res.statusText);
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

export function renderInspector(v) {
  const n = (v.nodes || []).find((x) => x.name === selected);
  if (!n) {
    replace('inspector', h('p', { class: 'muted' }, 'Select a node in the flow or the outline.'));
    return;
  }
  const row = (k, val) => val ? [h('dt', {}, k), h('dd', {}, val)] : [];
  replace('inspector',
    h('h3', {}, n.name),
    h('dl', {}, row('kind', n.kind), row('block', n.block), row('safety (config)', n.safety || 'left to the block'), row('crash behavior', n.risk),
      row('approval gate', n.gate || 'none'), row('problems', (n.problems || []).join('; '))),
    h('p', { class: 'muted small' }, 'This release shows the flow and does not change it.'));
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

export function render(v) {
  view = v;
  if (selected && !(v.nodes || []).some((n) => n.name === selected)) selected = '';
  $('file').textContent = v.name + ' (' + v.format + ')';
  $('modeline').textContent = v.mode;
  $('source').textContent = v.text;
  renderVerdict(v);
  renderCanvas(v);
  renderOutline(v);
  renderInspector(v);
  renderAssertions(v);
  renderProblems(v);
  announce(v.verdict.word + '. ' + v.verdict.line);
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
  try {
    render(await api('/api/doc'));
  } catch (e) {
    replace('verdict', h('p', { class: 'bad' }, 'Could not read the file: ' + e.message));
  }
}

if (typeof document !== 'undefined' && document.getElementById('verdict')) main();
