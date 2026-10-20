// spoof.mjs: names with characters that do not print are shown escaped, never drawn, on every surface
// of the page. plan refuses such names at load, so the server never sends one for a config; the page
// still writes them out, because a name can reach it by another road (a typed-mode registry names a
// block in Go, which can be any string). The script takes the real view of a config with placeholder
// names, replaces each placeholder in it with a name that holds a direction override, a zero-width
// space or a filler, and has the page draw that view: opened, each node selected, and the weakened
// banner. In each state it walks every text node, every title, aria-label, placeholder and alt, and
// every option's text, outside the source pane and the inputs an author edits in, and reports each
// character that does not print. It also checks the page's own helpers. It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const console_ = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.waitForSelector('#outline button');
  const until = (fn, what) => page.waitForFunction(fn, { timeout: 10000 }).catch(() => { throw new Error('timed out waiting for ' + what); });
  const hits = [];
  const walk = async (state) => {
    const found = await page.evaluate(() => {
      const bad = /[^\p{L}\p{M}\p{N}\p{P}\p{S} \t\n\r]/u;
      const res = [];
      const tw = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      for (let n = tw.nextNode(); n; n = tw.nextNode()) {
        const p = n.parentElement;
        if (!p || p.closest('#source, script, style')) continue;
        let t = n.textContent;
        if (p.id === 'live') t = t.replace(/ $/, ''); // announce() alternates a no-break space on a repeat
        const m = bad.exec(t);
        if (m) res.push({ where: p.tagName.toLowerCase() + (p.id ? '#' + p.id : '') + (p.getAttribute('class') ? '.' + p.getAttribute('class') : ''), text: t.slice(0, 80), cp: m[0].codePointAt(0).toString(16) });
      }
      for (const e of document.body.querySelectorAll('*')) {
        for (const a of ['aria-label', 'title', 'placeholder', 'alt', 'aria-description']) {
          const v = e.getAttribute(a);
          if (v && bad.test(v)) res.push({ where: e.tagName.toLowerCase() + '[' + a + ']', text: v.slice(0, 80), cp: bad.exec(v)[0].codePointAt(0).toString(16) });
        }
      }
      return res;
    });
    for (const f of found) hits.push({ state, ...f });
  };
  const r = {};
  // The page's helpers, on every class of character.
  r.helpers = await page.evaluate(async () => {
    const m = await import('/assets/editor.js');
    const cases = ['ch\u202egra', '\u200b', 'a\u200db', 'a\u00adb', 'a\u00a0b', 'a\u2028b', 'a\ufeffb', 'a\ue000b', 'a\u3164', 'a\uffa0b', '\u2800', 'a\u034fb', 'a\u17b4b', 'a\ufe0f', 'a\u180bb', '\u0301a'];
    const bad = /[^\p{L}\p{M}\p{N}\p{P}\p{S} \t\n\r]/u;
    const left = [];
    for (const c of cases) {
      const x = m.shown(c);
      if (bad.test(x) || x === c || !x.includes('\\u{')) left.push(c.codePointAt(0).toString(16) + ' -> ' + x);
      const e = m.h('p', { 'aria-label': c, title: c, placeholder: c }, c);
      if (bad.test(e.textContent) || bad.test(e.getAttribute('aria-label')) || bad.test(e.getAttribute('title')) || bad.test(e.getAttribute('placeholder'))) left.push('h: ' + c.codePointAt(0).toString(16));
    }
    const plain = ['manual review', 'caf\u00e9', '\u30ce\u30fc\u30c9', '\u05e9\u05dc\u05d5\u05dd', '\ud55c\uae00', 'a\tb\nc'];
    for (const c of plain) if (m.shown(c) !== c) left.push('plain changed: ' + c);
    return left;
  });
  // The real view of the config (placeholder names), with each placeholder replaced by a hostile name.
  const hostile = await page.evaluate(async () => {
    const token = (/(?:^#|&)t=([0-9a-f]+)/.exec(location.hash) || [])[1] || '';
    const text = document.getElementById('source').value;
    const res = await fetch('/api/report', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Bide-Token': token }, body: JSON.stringify({ text }) });
    let j = await res.text();
    const swap = { nodeA: 'ch\\u202egra', nodeB: '\\u200b', nodeC: 'o\\u3164k', blockA: 'bl\\u200bock', approvA: 'ap\\u202epr', flowZ: 'fl\\u202eow' };
    for (const [k, v] of Object.entries(swap)) j = j.split(k).join(v);
    const v = JSON.parse(j);
    v.weakening = { baseline: 'HEAD:f\u202e.json', weakened: ['ch\u202egra: approval 2 of 3 -> 1 of 3'], word: 'APPROVAL GATE WEAKENED', line: 'This is weaker', changes: ['ch\u202egra: gate changed'], strengthened: [] };
    const m = await import('/assets/editor.js');
    m.render(v, { keepSource: true });
    return v.nodes.map((n) => n.name);
  });
  r.nodes = hostile;
  await walk('rendered');
  const n = hostile.length;
  for (let i = 0; i < n; i++) {
    await page.evaluate((i) => document.querySelectorAll('#outline button')[i].click(), i);
    await until(() => document.getElementById('f-name'), 'the inspector');
    await walk('node ' + i + ' selected');
    if (i === 0) {
      r.title = await page.$eval('#inspector h3', (e) => e.textContent);
      r.warning = await page.$eval('#f-name-escaped', (e) => e.textContent);
      r.nameField = await page.$eval('#f-name', (e) => e.value);
    }
  }
  out = { ok: true, ...r, hits, console: console_ };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), console: console_ };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
