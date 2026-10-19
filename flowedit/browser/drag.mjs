// drag.mjs: drag to connect in real Chrome (flowedit/browser_test.go starts the server and runs this):
// drag from the small circle at a node's right edge onto another node to add an edge, the cancelled
// drags (onto the same node, onto nothing, Escape), a refused drag (a duplicate edge), and that the
// circle is a pointer shortcut only (hidden from assistive technology, not in the Tab order). It
// prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) offOrigin.push(r.url()); });
  await page.setViewport({ width: 1400, height: 1400 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  // The page scrolls when a field low on it is typed into; the flow is at the top.
  const center = async (sel) => { await page.evaluate(() => window.scrollTo(0, 0)); return page.$eval(sel, (e) => { const b = e.getBoundingClientRect(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 }; }); };
  const dragLines = () => page.$$eval('#canvas line.drag', (l) => l.length);
  const r = {};
  r.original = await source();

  // A node with nothing wired to it, to drag onto.
  await page.focus('#an-name');
  await page.keyboard.type('audit');
  await page.keyboard.press('Tab');
  await page.keyboard.type('audit_log');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: audit,'), null, 'the node');
  await atRest();
  r.withAudit = await source();
  r.ports = await page.$$eval('#canvas .port', (p) => p.length);
  r.portHidden = await page.$eval('#canvas .port', (p) => p.getAttribute('aria-hidden') === 'true' && !p.hasAttribute('tabindex'));
  r.tabStops = await page.$$eval('#canvas g.node', (g) => g.filter((n) => n.tabIndex === 0).length);

  const port = '#canvas g.node[data-id="receipt"] .port';
  const audit = '#canvas g.node[data-id="audit"]';

  // Drag receipt's circle onto audit.
  let a = await center(port);
  let b = await center(audit);
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move((a.x + b.x) / 2, (a.y + b.y) / 2, { steps: 5 });
  r.lineWhileDragging = await dragLines();
  await page.mouse.move(b.x, b.y, { steps: 5 });
  await page.mouse.up();
  await until(() => document.getElementById('source').value.includes('edge: [receipt, audit]'), null, 'the edge from the drag');
  await atRest();
  r.afterDrag = await source();
  r.lineAfter = await dragLines();
  r.live = await text('#live');
  r.selected = await text('#inspector h3');

  // Onto the same node, and onto nothing: cancelled, text unchanged.
  const before = await source();
  a = await center(port);
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move(a.x - 30, a.y, { steps: 3 });
  await page.mouse.up();
  await atRest();
  r.sameNodeUnchanged = (await source()) === before;
  r.sameNodeLive = await text('#live');
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move(a.x + 5, 5, { steps: 3 });
  await page.mouse.up();
  await atRest();
  r.nothingUnchanged = (await source()) === before;

  // Escape in the middle of a drag cancels it, even if the pointer is released over a node.
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  b = await center('#canvas g.node[data-id="validate"]');
  await page.mouse.move(b.x, b.y, { steps: 4 });
  r.lineBeforeEscape = await dragLines();
  await page.keyboard.press('Escape');
  await page.mouse.up();
  await atRest();
  r.lineAfterEscape = await dragLines();
  r.escapeUnchanged = (await source()) === before;
  r.escapeLive = await text('#live');

  // A refused drag (the edge is there already) says why and changes nothing.
  a = await center(port);
  b = await center(audit);
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move(b.x, b.y, { steps: 4 });
  await page.mouse.up();
  await until(() => /already there/.test(document.getElementById('live').textContent), null, 'the refusal');
  r.refusal = await text('#live');
  r.refusedUnchanged = (await source()) === before;
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
