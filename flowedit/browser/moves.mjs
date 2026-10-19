// moves.mjs: reorder and the flow settings in real Chrome (flowedit/browser_test.go starts the server
// and runs this): move a node up and down from the inspector, move a wiring element from the outline's
// wiring list, set the flow's name and the documentation types, and a move the engine refuses, all by
// controls a keyboard reaches. It prints one JSON line with what it saw.
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
  const focused = () => page.evaluate(() => document.activeElement?.id);
  const outline = () => page.$$eval('#outline button .nm', (b) => b.map((x) => x.textContent));
  const r = {};
  r.original = await source();
  r.outlineBefore = await outline();

  // Move a node up from the inspector, with the keyboard.
  await page.click('#outline button[data-node="receipt"]');
  await until(() => document.getElementById('f-up'), null, 'the move buttons');
  r.position = await text('#f-pos');
  r.upLabel = await page.$eval('#f-up', (e) => e.getAttribute('aria-label'));
  r.downDisabledAtEnd = await page.$eval('#f-down', (e) => e.disabled);
  await page.focus('#f-up');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.indexOf('name: receipt') < document.getElementById('source').value.indexOf('name: charge'), null, 'the move up');
  r.afterUp = await source();
  r.outlineAfterUp = await outline();
  r.positionAfterUp = await text('#f-pos');
  r.focusAfterUp = await focused();
  r.liveAfterUp = await text('#live');
  // Down again restores the text; the undo button does too.
  await page.click('#f-down');
  await until(() => document.getElementById('source').value.indexOf('name: charge') < document.getElementById('source').value.indexOf('name: receipt'), null, 'the move down');
  r.afterDown = await source();
  r.stateAfterDown = await text('#state-tag');

  // The first node cannot move up, and with its edge buttons disabled the page does not ask.
  await page.click('#outline button[data-node="validate"]');
  await until(() => document.getElementById('f-up'), null, 'the first node form');
  r.firstUpDisabled = await page.$eval('#f-up', (e) => e.disabled);

  // Wiring: move the second element up from the wiring list.
  r.wiringItems = await page.$$eval('#wiring button', (b) => b.map((x) => x.getAttribute('data-wire')));
  r.wireStops = await page.$$eval('#wiring button', (b) => b.filter((x) => x.tabIndex === 0).length);
  r.moveBeforeSelect = await page.$eval('#w-up', (e) => e.disabled);
  await page.click('#wiring button[data-wire="1"]');
  await until(() => document.getElementById('w-pos')?.textContent.includes('2 of 2'), null, 'the wiring selection');
  r.wirePos = await text('#w-pos');
  r.wireLabel = await page.$eval('#w-up', (e) => e.getAttribute('aria-label'));
  await page.focus('#w-up');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.indexOf('edge: [charge, receipt]') < document.getElementById('source').value.indexOf('edge: [validate, charge]'), null, 'the wiring move');
  r.afterWiring = await source();
  r.focusAfterWiring = await focused();
  r.upDisabledAfter = await page.$eval('#w-up', (e) => e.disabled);
  r.wirePosAfter = await text('#w-pos');

  // The flow settings are in a disclosure: closed they are one Tab stop, open they are the fields.
  r.settingsOpenAtStart = await page.$eval('#flow-settings', (e) => e.open);
  await page.focus('#flow-settings summary');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('flow-settings').open, null, 'the flow settings to open');

  // The flow's name and the documentation types.
  await page.$eval('#fs-flow', (e) => { e.value = ''; });
  await page.focus('#fs-flow');
  await page.keyboard.type('refund v2');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('flow: refund v2'), null, 'the flow name');
  r.afterFlow = await source();
  r.focusAfterFlow = await focused();
  await page.focus('#fs-in');
  await page.keyboard.type('Order');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('in: Order'), null, 'the input type');
  r.afterIn = await source();
  await page.$eval('#fs-in', (e) => { e.value = ''; });
  await page.focus('#fs-in');
  await page.keyboard.press('Enter');
  await until(() => !document.getElementById('source').value.includes('in: Order'), null, 'the input type to be removed');
  r.afterInCleared = await source();
  // A refused value is said so, under the form.
  await page.$eval('#fs-flow', (e) => { e.value = ''; });
  await page.focus('#fs-flow');
  await page.keyboard.press('Enter');
  await until(() => /cannot be empty/.test(document.getElementById('live').textContent), null, 'the refusal');
  r.refusal = await text('#live');
  r.sourceAfterRefusal = await source();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
