// wiring.mjs: edges, a switch, its arms and its else in real Chrome (flowedit/browser_test.go starts the
// server and runs this): add and remove an edge, make a node a switch, add, change, move and remove
// arms, set and clear the else, remove the switch, all by controls a keyboard reaches. It prints one
// JSON line with what it saw.
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
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  const focused = () => page.evaluate(() => document.activeElement?.id);
  const select = async (name) => { await page.click(`#outline button[data-node="${name}"]`); await until(() => document.getElementById('connections'), null, 'the connections form'); await atRest(); };
  const type = async (sel, value) => { await page.$eval(sel, (e) => { e.value = ''; }); await page.focus(sel); await page.keyboard.type(value); };
  const r = {};
  r.original = await source();

  // A node to wire.
  await page.focus('#an-name');
  await page.keyboard.type('audit');
  await page.keyboard.press('Tab');
  await page.keyboard.type('audit_log');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: audit,'), null, 'the node');
  r.withAudit = await source();

  // An edge from receipt to audit, with the keyboard; then removed.
  await select('receipt');
  r.edgesOutBefore = await page.$$eval('#c-outs li', (l) => l.length);
  r.inLine = await text('#c-in');
  await page.select('#c-edge-to', 'audit');
  await page.focus('#c-edge-add');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('edge: [receipt, audit]'), null, 'the edge');
  r.afterEdge = await source();
  r.focusAfterEdge = await focused();
  r.outsAfter = await page.$$eval('#c-outs li span', (l) => l.map((x) => x.textContent));
  r.liveEdge = await text('#live');
  await page.click('#c-rm-edge-0');
  await until(() => !document.getElementById('source').value.includes('edge: [receipt, audit]'), null, 'the edge to go');
  r.afterRemoveEdge = await source();

  // A refusal: an edge that is already there.
  await select('validate');
  await page.select('#c-edge-to', 'charge');
  await page.click('#c-edge-add');
  await until(() => /already there/.test(document.getElementById('op-error')?.textContent || ''), null, 'the refusal');
  r.dupEdge = await text('#op-error');

  // A switch on audit: first arm, a second, change, move, else, remove an arm, remove the switch.
  await select('audit');
  r.switchOffered = await page.$eval('#c-sw-pred', () => true);
  await type('#c-sw-pred', 'big');
  await page.select('#c-sw-to', 'receipt');
  await page.click('#c-sw-add');
  await until(() => document.getElementById('c-arms'), null, 'the switch');
  await atRest();
  r.afterSwitch = await source();
  r.armsOne = await page.$$eval('#c-arms li', (l) => l.length);
  r.rmDisabledOnly = await page.$eval('#c-arm-rm-0', (b) => b.disabled);
  await type('#c-arm-pred', 'small');
  await page.select('#c-arm-to', 'charge');
  await page.click('#c-arm-add');
  await until(() => document.querySelectorAll('#c-arms li').length === 2, null, 'the second arm');
  await atRest();
  r.afterArm = await source();
  await page.select('#c-arm-to-1', 'validate');
  await page.click('#c-arm-set-1');
  await until(() => document.getElementById('source').value.includes('to: validate'), null, 'the arm to change');
  await atRest();
  r.afterSetArm = await source();
  await page.click('#c-arm-up-1');
  await until(() => document.getElementById('source').value.indexOf('pred: small') < document.getElementById('source').value.indexOf('pred: big'), null, 'the arm to move');
  await atRest();
  r.afterMoveArm = await source();
  r.focusAfterMove = await focused();
  await page.select('#c-else', 'charge');
  await page.click('#c-else-set');
  await until(() => document.getElementById('source').value.includes('else: charge'), null, 'the else');
  await atRest();
  r.afterElse = await source();
  await page.select('#c-else', '');
  await page.click('#c-else-set');
  await until(() => !document.getElementById('source').value.includes('else:'), null, 'the else to go');
  await atRest();
  await page.click('#c-arm-rm-0');
  await until(() => document.querySelectorAll('#c-arms li').length === 1, null, 'an arm to go');
  await atRest();
  r.afterRemoveArm = await source();
  r.lastRmDisabled = await page.$eval('#c-arm-rm-0', (b) => b.disabled);
  await page.click('#c-sw-rm');
  await until(() => !document.getElementById('source').value.includes('switch: audit'), null, 'the switch to go');
  await atRest();
  r.afterRemoveSwitch = await source();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
