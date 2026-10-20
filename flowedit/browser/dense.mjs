// dense.mjs: a drag handle in a dense flow, in real Chrome (flowedit/browser_test.go starts the server and
// runs this). A port's hit area is 44 CSS px, and two nodes of a row are 44 drawing units apart, so with the
// drawing scaled to half or less the area would reach over the neighbour's body. It must not: a press on the
// neighbour selects it, a drop onto it lands on it, and a drag from the port still starts. It prints one
// JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
const r = {};
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) offOrigin.push(r.url()); });
  await page.setViewport({ width: 1400, height: 2600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  const rect = (sel) => page.$eval(sel, (e) => { const b = e.getBoundingClientRect(); return { x: b.x, y: b.y, w: b.width, h: b.height }; });
  r.original = await source();
  // The svg is made narrow until the gap between the two nodes of the row is under 22 CSS px (a hit
  // area reaches 22 px from the port).
  for (const width of [900, 600, 450, 380, 330, 300]) {
    await page.setViewport({ width, height: 2600 });
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.waitForFunction(() => document.documentElement.dataset.busy === '0');
    const b = await rect('#canvas g.node[data-id="b"] rect.box');
    const c = await rect('#canvas g.node[data-id="c"] rect.box');
    r.gapPx = c.x - (b.x + b.w);
    r.viewportWidth = width;
    if (r.gapPx < 18) break;
  }
  const b = await rect('#canvas g.node[data-id="b"] rect.box');
  const c = await rect('#canvas g.node[data-id="c"] rect.box');
  const hit = await rect('#canvas g.node[data-id="b"] .port-hit-box');
  r.hitW = hit.w;
  r.hitRight = hit.x + hit.w;
  r.neighbourLeft = c.x;
  r.overlap = Math.max(0, Math.min(hit.x + hit.w, c.x + c.w) - Math.max(hit.x, c.x)) * Math.max(0, Math.min(hit.y + hit.h, c.y + c.h) - Math.max(hit.y, c.y));
  // The port is still pressable where it is drawn.
  const port = await rect('#canvas g.node[data-id="b"] .port');
  const pc = { x: port.x + port.w / 2, y: port.y + port.h / 2 };
  r.underPort = await page.evaluate((p) => document.elementFromPoint(p.x, p.y)?.className?.baseVal ?? document.elementFromPoint(p.x, p.y)?.className, pc);
  // A press just inside the neighbour's left edge selects the neighbour.
  r.topAtNeighbourEdge = await page.evaluate((p) => { const e = document.elementFromPoint(p.x, p.y); return e && e.closest('g.node') ? e.closest('g.node').getAttribute('data-id') : ''; }, { x: c.x + 2, y: c.y + c.h / 2 });
  await page.mouse.click(c.x + 2, c.y + c.h / 2);
  await until(() => document.querySelector('#inspector h3'), null, 'the inspector');
  await atRest();
  r.selectedByEdgeClick = await text('#inspector h3');
  // The press on b's own body selects b.
  await page.mouse.click(b.x + b.w / 2, b.y + b.h / 2);
  await atRest();
  r.selectedBody = await text('#inspector h3');
  // A drag from b's port onto the neighbour's left edge lands on the neighbour.
  await page.mouse.move(pc.x, pc.y);
  await page.mouse.down();
  await page.mouse.move((pc.x + c.x) / 2, pc.y, { steps: 3 });
  await page.mouse.move(c.x + 2, c.y + c.h / 2, { steps: 3 });
  await page.mouse.up();
  await until(() => document.getElementById('source').value.includes('edge: [b, c]'), null, 'the edge from the drag');
  await atRest();
  r.afterDrag = await source();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, ...r, error: String(e && e.message || e) + '; the live region says: ' + live + '; so far ' + JSON.stringify(r), console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
