// typed.mjs: typed mode in real Chrome (flowedit/browser_test.go starts a typed server and runs this):
// the source label and the digest, the palette of the program's blocks (a search, the "fits the
// selected node" filter, one Tab stop, the two actions), the "(types differ)" hint, and the server's
// refusal of a connection that does not fit, in the form and by drag, in the same words. It prints one
// JSON line.
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
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  const focusedId = () => page.evaluate(() => document.activeElement?.id || document.activeElement?.getAttribute('data-block') || '');
  const select = async (name) => { await page.click(`#outline button[data-node="${name}"]`); await until(() => document.getElementById('connections'), null, 'the connections form'); await atRest(); };
  const center = async (sel) => { await page.evaluate(() => window.scrollTo(0, 0)); return page.$eval(sel, (e) => { const b = e.getBoundingClientRect(); return { x: b.x + b.width / 2, y: b.y + b.height / 2 }; }); };
  const r = {};
  r.original = await source();

  // What the check rests on, and the digest.
  r.tag = await text('#typed-tag');
  r.tagHidden = await page.$eval('#typed-tag', (e) => e.hidden);
  r.mode = await text('#modeline');
  r.digestSummary = await text('#digest summary');
  r.digestFull = await text('#digest code');
  r.palette = await page.$$eval('#palette button', (b) => b.map((x) => x.getAttribute('data-block')));
  r.count = await text('#pal-count');
  r.firstLabel = await page.$eval('#palette button', (b) => b.innerText.replace(/\s+/g, ' ').trim());
  r.skipLinkShown = await page.$eval('a[data-skip="blocks"]', (a) => !a.hidden);

  // One Tab stop in the list, and the two actions after it.
  r.tabStops = await page.$$eval('#palette button', (b) => b.filter((x) => x.tabIndex === 0).length);
  await page.focus('#pal-q');
  const order = [];
  for (let i = 0; i < 4; i++) { await page.keyboard.press('Tab'); order.push(await focusedId()); }
  r.tabOrder = order;
  await page.focus('#palette button[tabindex="0"]');
  await page.keyboard.press('ArrowDown');
  r.afterArrow = await focusedId();
  r.currentRows = await page.$$eval('#palette button[aria-current="true"]', (b) => b.map((x) => x.getAttribute('data-block')));

  // The search.
  await page.type('#pal-q', 'gat');
  r.searchRows = await page.$$eval('#palette button', (b) => b.map((x) => x.getAttribute('data-block')));
  await page.$eval('#pal-q', (e) => { e.value = ''; e.dispatchEvent(new Event('input', { bubbles: true })); });

  // "Fits the selected node": off by default, then on for validate (takes and gives an order).
  r.fitOffByDefault = await page.$eval('#pal-fit', (c) => !c.checked);
  await select('validate');
  r.inspectorTypes = await page.$$eval('#inspector dl dt', (d) => d.map((x) => x.textContent + ': ' + x.nextSibling.textContent).filter((x) => /^(takes|gives)/.test(x)));
  r.allRows = await page.$$eval('#palette button', (b) => b.length);
  await page.click('#pal-fit');
  r.fitRows = await page.$$eval('#palette button', (b) => b.map((x) => x.getAttribute('data-block')));
  r.fitCount = await text('#pal-count');
  // The filter follows the selection: receipt takes an order and gives a string.
  await select('receipt');
  r.fitRowsReceipt = await page.$$eval('#palette button', (b) => b.map((x) => x.getAttribute('data-block')));
  await page.click('#pal-fit');

  // Add a node with a block that fits nothing: the form is filled, the name is made unique, nothing is written.
  await page.click('#palette button[data-block="count"]');
  await page.click('#pal-add');
  r.addFormBlock = await page.$eval('#an-block', (e) => e.value);
  r.addFormName = await page.$eval('#an-name', (e) => e.value);
  r.addFocus = await focusedId();
  r.addNothingWritten = (await source()) === r.original;
  await page.focus('#an-add');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: count,'), null, 'the node');
  await atRest();
  r.withCount = await source();
  // A second node with the same block gets an unused name.
  await page.click('#palette button[data-block="count"]');
  await page.click('#pal-add');
  r.secondName = await page.$eval('#an-name', (e) => e.value);
  await page.$eval('#an-name', (e) => { e.value = ''; });
  await page.$eval('#an-block', (e) => { e.value = ''; });

  // The hint on the form's targets, by string equality of type names; the target stays selectable.
  await select('validate');
  r.edgeOptions = await page.$$eval('#c-edge-to option', (o) => o.map((x) => x.textContent));
  r.edgeValues = await page.$$eval('#c-edge-to option', (o) => o.map((x) => x.value));
  // A refused edge by the form: the server's reason, nothing written.
  const before = await source();
  await page.select('#c-edge-to', 'count');
  await page.click('#c-edge-add');
  await until(() => /Not applied: the types do not fit/.test(document.getElementById('op-error').textContent), null, 'the refusal');
  await atRest();
  r.formRefusal = await text('#op-error');
  r.formLive = await text('#live');
  r.formUnchanged = (await source()) === before;

  // The same edge by drag: the same words, nothing written.
  const a = await center('#canvas g.node[data-id="validate"] .port');
  const b = await center('#canvas g.node[data-id="count"]');
  await page.mouse.move(a.x, a.y);
  await page.mouse.down();
  await page.mouse.move(b.x, b.y, { steps: 5 });
  await page.mouse.up();
  await until(() => /Not applied: the types do not fit/.test(document.getElementById('live').textContent), null, 'the refusal of the drag');
  await atRest();
  r.dragRefusal = await text('#op-error');
  r.dragLive = await text('#live');
  r.dragUnchanged = (await source()) === before;

  // Use a block that does not fit: refused. One that fits: written.
  await select('receipt');
  await page.click('#palette button[data-block="shout"]');
  r.useLabel = await text('#pal-use');
  await page.click('#pal-use');
  await until(() => /Not applied: the types do not fit/.test(document.getElementById('op-error')?.textContent || ''), null, 'the refusal of a block');
  await atRest();
  r.useRefusal = await text('#op-error');
  await page.click('#palette button[data-block="plain"]');
  await page.click('#pal-use');
  await until(() => /name: receipt, block: plain/.test(document.getElementById('source').value), null, 'the block');
  await atRest();
  r.afterUse = await source();
  r.useFocus = await focusedId();
  r.useLive = await text('#live');
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
