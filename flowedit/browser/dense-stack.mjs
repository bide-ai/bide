// dense-stack.mjs: ports of nodes stacked in a column, at a small scale, in real Chrome (flowedit/browser_test.go
// starts the server and runs this). Each node's circle must be inside its own hit box and be the thing a press at its
// centre reaches, and a drag from it must start; nodes of equal width that sit above and below each other must not
// cut the box off at the circle. It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
const r = {};
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (q) => failed.push(`${q.method()} ${q.url()} ${q.failure()?.errorText}`));
  page.on('request', (q) => { if (!q.url().startsWith(origin) && !q.url().startsWith('data:')) offOrigin.push(q.url()); });
  await page.setViewport({ width: 420, height: 3000 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.waitForFunction(() => document.documentElement.dataset.busy === '0');
  r.scale = await page.$eval('#canvas svg', (s) => s.getScreenCTM().a);
  r.nodes = await page.$$eval('#canvas g.node', (g) => g.map((n) => n.getAttribute('data-id')));
  r.ports = [];
  for (const name of r.nodes) {
    const sel = `#canvas g.node[data-id="${name}"]`;
    const info = await page.evaluate((s) => {
      const g = document.querySelector(s);
      const c = g.querySelector('.port').getBoundingClientRect();
      const h = g.querySelector('.port-hit-box').getBoundingClientRect();
      const cx = c.x + c.width / 2;
      const cy = c.y + c.height / 2;
      const top = document.elementFromPoint(cx, cy);
      return { cx, cy, inside: cx >= h.x && cx <= h.x + h.width && cy >= h.y && cy <= h.y + h.height, topNode: top && top.closest('g.node') ? top.closest('g.node').getAttribute('data-id') : '', topIsHit: !!(top && top.classList.contains('port-hit-box')) };
    }, sel);
    await page.evaluate((y) => window.scrollTo(0, Math.max(0, y - 300)), info.cy);
    const live = await page.evaluate((s) => { const c = document.querySelector(s + ' .port').getBoundingClientRect(); return { x: c.x + c.width / 2, y: c.y + c.height / 2 }; }, sel);
    await page.mouse.move(live.x, live.y);
    await page.mouse.down();
    await page.mouse.move(live.x + 25, live.y + 25, { steps: 3 });
    const lines = await page.$$eval('#canvas line.drag', (l) => l.length);
    await page.mouse.up();
    await page.waitForFunction(() => document.documentElement.dataset.busy === '0');
    r.ports.push({ name, inside: info.inside, topNode: info.topNode, topIsHit: info.topIsHit, dragLines: lines });
  }
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, ...r, error: String(e && e.message || e), console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
