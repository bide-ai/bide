// taint.mjs: the static taint view in real Chrome (flowedit/taint_browser_test.go starts a typed editor over
// a flow whose source runs last, and runs this). It edits the text so that the side-effecting node newly reads
// the source, and measures what the page then shows: the words under the nodes and in the outline, the
// inspector's Taint section with its path, the summary panel and its buttons, the banner and the live region,
// the skip link and the keyboard path, the ring patterns, forced colors, and the contrast of the words in each
// theme. It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) offOrigin.push(r.url()); });
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  const focusedId = () => page.evaluate(() => document.activeElement?.id || document.activeElement?.getAttribute('data-taint-node') || document.activeElement?.getAttribute('data-node') || document.activeElement?.tagName || '');
  const r = {};
  await atRest();

  // As opened: fetch runs last, so only fetch has anything to say, and the panel does not say more.
  r.openedTags = await page.$$eval('#canvas .ntaint', (e) => e.map((x) => x.textContent));
  r.openedRings = await page.$$eval('#canvas .tring', (e) => e.map((x) => x.getAttribute('class')));
  r.openedLine = await text('#taint-line');
  r.openedPanelHidden = await page.$eval('#taint-panel', (e) => e.hidden);
  r.openedSkipHidden = await page.$eval('#skip-taint', (e) => e.hidden);
  r.openedBanner = await page.$eval('#weakened', (e) => e.textContent);

  // The edit: fetch runs first. The text is the one the test file holds as `exposed`.
  const exposed = env.EXPOSED;
  await page.$eval('#source', (e, t) => { e.value = t; e.dispatchEvent(new Event('input', { bubbles: true })); }, exposed);
  await until(() => document.querySelector('#weakened .vword'), null, 'the banner');
  await atRest();
  r.bannerWord = await text('#weakened .vword');
  r.bannerRole = await page.$eval('#weakened section', (e) => e.getAttribute('role'));
  r.bannerDetail = await page.$$eval('#weakened .vdetail', (e) => e.map((x) => x.textContent));
  r.live = await text('#live');
  r.line = await text('#taint-line');
  r.counts = await page.$$eval('#taint-counts li', (e) => e.map((x) => x.textContent));
  r.exposedButtons = await page.$$eval('#taint-exposed button', (e) => e.map((x) => x.getAttribute('data-taint-node')));
  r.unknownButtons = await page.$$eval('#taint-unknown button', (e) => e.map((x) => x.getAttribute('data-taint-node')));
  r.sourceButtons = await page.$$eval('#taint-sources button', (e) => e.map((x) => x.getAttribute('data-taint-node')));
  r.caveat = await text('#taint-caveat');
  r.tags = await page.$$eval('#canvas .ntaint', (e) => e.map((x) => x.textContent));
  r.rings = await page.$$eval('#canvas g.node', (g) => Object.fromEntries(g.map((n) => [n.getAttribute('data-id'), [...n.querySelectorAll('.tring')].map((x) => x.getAttribute('class'))])));
  r.titles = await page.$$eval('#canvas g.node', (g) => Object.fromEntries(g.map((n) => [n.getAttribute('data-id'), n.querySelector('title').textContent])));
  r.taintwDisplay = await page.$eval('#outline .taintw', (e) => getComputedStyle(e).display);
  r.outline = await page.$$eval('#outline button[data-node]', (b) => Object.fromEntries(b.map((x) => [x.getAttribute('data-node'), x.innerText.replace(/\s+/g, ' ').trim()])));

  // A node is its own button; the drawn words are not read twice (they are hidden from assistive technology).
  r.tagsHidden = await page.$$eval('#canvas .ntaint', (e) => e.every((x) => x.getAttribute('aria-hidden') === 'true' || x.closest('[aria-hidden="true"]') !== null));

  // The summary panel: a button selects the node, by mouse and by keyboard, and the inspector shows the path.
  await page.click('#taint-exposed button[data-taint-node="refund"]');
  await atRest();
  r.selectedByPanel = await text('#inspector h3');
  r.inspectorVerdict = await text('#taint-verdict');
  r.inspectorPath = await text('#taint-path');
  r.inspectorSection = await page.$eval('#taint-section', (e) => e.innerText.replace(/\s+/g, ' ').trim());
  r.liveAfterSelect = await text('#live');
  await page.focus('#taint-sources button');
  await page.keyboard.press('Enter');
  await atRest();
  r.selectedByKeyboard = await text('#inspector h3');
  r.panelFocusAfter = await focusedId();
  r.fetchSection = await page.$eval('#taint-section', (e) => e.innerText.replace(/\s+/g, ' ').trim());
  // A node with no verdict says what the view knows, and nothing more.
  await page.click('#outline button[data-node="notify"]');
  await atRest();
  r.notifySection = await page.$eval('#taint-section', (e) => e.innerText.replace(/\s+/g, ' ').trim());
  // The skip link lands on the panel's heading.
  await page.focus('#skip-taint');
  await page.keyboard.press('Enter');
  r.skipFocus = await focusedId();

  // One Tab stop each in the panel lists is not claimed: every button is reachable by Tab, in order.
  await page.focus('#h-taint');
  const order = [];
  for (let i = 0; i < 4; i++) { await page.keyboard.press('Tab'); order.push(await focusedId()); }
  r.tabOrder = order;

  // The rings: each class has its own dash pattern, so the states differ without color.
  r.patterns = await page.evaluate(() => {
    const svg = document.querySelector('#canvas svg');
    const g = svg.querySelector('g.node');
    const out = {};
    for (const c of ['reads', 'escalates', 'refused', 'exempt', 'source']) {
      const e = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
      e.setAttribute('class', 'tring tr-' + c);
      g.append(e);
      const s = getComputedStyle(e);
      out[c] = { dash: s.strokeDasharray, width: s.strokeWidth, stroke: s.stroke };
      e.remove();
    }
    return out;
  });

  // Forced colors: the system's colors win, the ring is still drawn, and the words are CanvasText.
  const painted = () => page.evaluate(() => {
    const probe = (value) => { const e = document.createElement('i'); e.style.color = value; document.body.append(e); const c = getComputedStyle(e).color; e.remove(); return c; };
    const ring = document.querySelector('#canvas .tring');
    const tag = document.querySelector('#canvas .ntaint');
    return { ringStroke: ring ? getComputedStyle(ring).stroke : '', ringWidth: ring ? getComputedStyle(ring).strokeWidth : '', tagFill: tag ? getComputedStyle(tag).fill : '', canvasText: probe('CanvasText') };
  });
  await page.emulateMediaFeatures([{ name: 'forced-colors', value: 'active' }]);
  r.forced = await painted();
  await page.emulateMediaFeatures([{ name: 'forced-colors', value: 'none' }]);

  // Contrast of the words in each theme: the tag under a node against the canvas, the outline's line against its button, the ring against the canvas.
  const measure = () => page.evaluate(() => {
    const rgb = (s) => { const m = /rgba?\(([^)]+)\)/.exec(s); if (!m) return null; const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number); return p.length === 3 ? [...p, 1] : p; };
    const lum = ([r, g, b]) => { const f = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
    const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
    const under = (el) => { for (let e = el; e; e = e.parentElement) { const c = rgb(getComputedStyle(e).backgroundColor); if (c && c[3] === 1) return c; } return rgb(getComputedStyle(document.body).backgroundColor) || [255, 255, 255, 1]; };
    const tag = document.querySelector('#canvas .ntaint');
    const ring = document.querySelector('#canvas .tring');
    const words = document.querySelector('#outline .taintw');
    const wrap = document.getElementById('canvas');
    return {
      tag: ratio(rgb(getComputedStyle(tag).fill), under(wrap)),
      ring: ratio(rgb(getComputedStyle(ring).stroke), under(wrap)),
      words: ratio(rgb(getComputedStyle(words).color), under(words.parentElement)),
      theme: document.documentElement.getAttribute('data-theme') || '',
    };
  });
  r.themes = {};
  for (const [k, f, t] of [['light', [], 'light'], ['dark', [], 'dark'], ['high-contrast', [], 'high-contrast'], ['prefers-contrast', [{ name: 'prefers-contrast', value: 'more' }], null]]) {
    await page.emulateMediaFeatures(f);
    await page.evaluate((th) => { if (th) document.documentElement.setAttribute('data-theme', th); else document.documentElement.removeAttribute('data-theme'); }, t);
    await page.evaluate(() => new Promise((res) => requestAnimationFrame(() => requestAnimationFrame(res))));
    r.themes[k] = await measure();
  }
  await page.emulateMediaFeatures([]);

  // Undo the edit: the banner goes, and the page says the taint changed.
  await page.$eval('#source', (e, t) => { e.value = t; e.dispatchEvent(new Event('input', { bubbles: true })); }, env.OPENED);
  await until(() => !document.querySelector('#weakened .vword'), null, 'the banner to go');
  await atRest();
  r.afterBanner = await page.$eval('#weakened', (e) => e.textContent);
  r.afterLive = await text('#live');
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
