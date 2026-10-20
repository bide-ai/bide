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
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) offOrigin.push(r.url()); });
  await page.setViewport({ width: 1400, height: 1400, hasTouch: true });
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
  // A drag that loses its pointer capture ends with no change and no line left on the page.
  a = await center(port);
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move(a.x + 20, a.y + 20, { steps: 3 });
  r.lineBeforeLostCapture = await dragLines();
  await page.$eval(port, (p) => p.releasePointerCapture(1));
  await page.mouse.up();
  await atRest();
  r.lineAfterLostCapture = await dragLines();
  r.lostCaptureUnchanged = (await source()) === before;

  // A redraw in the middle of a drag (the source is edited) ends the drag, and the Escape that
  // follows is left alone: the drag's key handler must not outlive it.
  a = await center(port);
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move(a.x + 20, a.y + 20, { steps: 3 });
  r.lineBeforeRedraw = await dragLines();
  await page.evaluate(() => {
    const s = document.getElementById('source');
    s.value = s.value + '\n# redraw';
    s.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await until(() => document.querySelectorAll('#canvas line.drag').length === 0, null, 'the redraw to end the drag');
  await page.mouse.up();
  await atRest();
  await page.evaluate(() => { window.__escPrevented = null; document.addEventListener('keydown', (e) => { if (e.key === 'Escape') window.__escPrevented = e.defaultPrevented; }); });
  const liveBefore = await text('#live');
  await page.keyboard.press('Escape');
  r.escapeAnnounced = (await text('#live')) !== liveBefore;
  r.escapePrevented = await page.evaluate(() => window.__escPrevented);
  r.portsAfterRedraw = await page.$$eval('#canvas .port', (p) => p.length);

  // Touch. The page is made short so that it scrolls. A touch that starts on the circle drags (it is
  // not taken for a scroll); a touch that starts anywhere else on the flow still scrolls the page.
  await page.focus('#an-name');
  await page.keyboard.type('ledger');
  await page.keyboard.press('Tab');
  await page.keyboard.type('ledger_log');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: ledger,'), null, 'the second node');
  await atRest();
  await page.setViewport({ width: 1400, height: 700, hasTouch: true });
  // Scrolling by touch can fling on after the finger lifts; wait until the page has held still for ten frames.
  const still = () => page.evaluate(() => new Promise((res) => { let last = -1; let n = 0; const f = () => { n = window.scrollY === last ? n + 1 : 0; last = window.scrollY; if (n >= 10) res(last); else requestAnimationFrame(f); }; f(); }));
  const toFlow = async () => { await still(); const y = await page.evaluate(() => { window.scrollTo(0, document.querySelector('#canvas svg').getBoundingClientRect().top + window.scrollY - 20); return window.scrollY; }); await still(); return y; };
  r.flowScroll = await toFlow();
  r.hitSize = await page.$eval('#canvas g.node[data-id="receipt"] .port-hit-box', (e) => { const b = e.getBoundingClientRect(); return Math.min(b.width, b.height); });
  const touch = async (from, to) => {
    await page.touchscreen.touchStart(from.x, from.y);
    for (let i = 1; i <= 6; i++) await page.touchscreen.touchMove(from.x + (to.x - from.x) * i / 6, from.y + (to.y - from.y) * i / 6);
    await page.touchscreen.touchEnd();
  };
  r.pageScrollable = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight > window.scrollY + 100);
  const corner = await page.$eval('#canvas svg', (e) => { const b = e.getBoundingClientRect(); return { x: b.right - 6, y: b.top + 6 }; });
  r.cornerIsSvg = await page.evaluate((c) => document.elementFromPoint(c.x, c.y).closest('g.node') === null && !!document.elementFromPoint(c.x, c.y).closest('#canvas svg'), corner);
  await touch(corner, { x: corner.x, y: corner.y - 150 });
  await page.waitForFunction((y) => window.scrollY > y, { timeout: 5000 }, r.flowScroll).catch(() => {});
  await still();
  r.touchScrolled = (await page.evaluate(() => window.scrollY)) - r.flowScroll;
  r.dragStartedByScroll = (await source()).includes('edge: [receipt, ledger]');
  await toFlow();
  const here = (sel) => page.$eval(sel, (e) => { const b = e.getBoundingClientRect(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 }; });
  const tp = await here('#canvas g.node[data-id="receipt"] .port');
  const tt = await here('#canvas g.node[data-id="ledger"]');
  await touch(tp, tt);
  await until(() => document.getElementById('source').value.includes('edge: [receipt, ledger]'), null, 'the edge from the touch drag');
  await atRest();
  await page.setViewport({ width: 1400, height: 500, hasTouch: true });

  // The page scrolled, so that the nodes sit away from where they are at the top: the drop still lands.
  const pos = await page.evaluate(() => {
    const y = (sel) => { const b = document.querySelector(sel).getBoundingClientRect(); return b.y + window.scrollY + b.height / 2; };
    return { port: y('#canvas g.node[data-id="validate"] .port'), target: y('#canvas g.node[data-id="ledger"]') };
  });
  await page.evaluate((y) => window.scrollTo(0, Math.max(0, y)), Math.min(pos.port, pos.target) - 20);
  await still();
  r.scrolledTo = await page.evaluate(() => window.scrollY);
  r.belowFirstViewport = Math.min(pos.port, pos.target) > 500;
  const sp = await page.$eval('#canvas g.node[data-id="validate"] .port', (e) => { const b = e.getBoundingClientRect(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 }; });
  const st = await page.$eval('#canvas g.node[data-id="ledger"]', (e) => { const b = e.getBoundingClientRect(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 }; });
  await page.mouse.move(sp.x, sp.y);
  await page.mouse.down();
  await page.mouse.move(st.x, st.y, { steps: 5 });
  await page.mouse.up();
  await until(() => document.getElementById('source').value.includes('edge: [validate, ledger]'), null, 'the edge from the scrolled drag');
  await atRest();
  // The window is made narrower after the flow is drawn: the svg is scaled down, and the hit area is
  // made larger in drawing units to stay at least 44 CSS px.
  const hitNow = () => page.$eval('#canvas g.node[data-id="receipt"] .port-hit-box', (e) => { const b = e.getBoundingClientRect(); return Math.min(b.width, b.height); });
  const svgWidth = () => page.$eval('#canvas svg', (e) => e.getBoundingClientRect().width);
  r.svgWidthWide = await svgWidth();
  await page.setViewport({ width: 500, height: 500, hasTouch: true });
  await page.waitForFunction((w) => document.querySelector('#canvas svg').getBoundingClientRect().width < w, { timeout: 5000 }, r.svgWidthWide).catch(() => {});
  r.svgWidthNarrow = await svgWidth();
  await page.waitForFunction(() => { const b = document.querySelector('#canvas g.node[data-id="receipt"] .port-hit-box').getBoundingClientRect(); return Math.min(b.width, b.height) >= 43.99; }, { timeout: 3000 }).catch(() => {});
  r.hitSizeNarrow = await hitNow();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
