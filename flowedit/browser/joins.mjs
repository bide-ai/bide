// joins.mjs: a join and a loop bound in real Chrome (flowedit/browser_test.go starts the server and runs
// this): add a join from the disclosure, set its merge, add, reorder and remove its inputs, gate it,
// give an arm a loop bound, and remove the join through the dialog that lists what goes. It prints one
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
  await page.setViewport({ width: 1400, height: 1800 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  const focused = () => page.evaluate(() => document.activeElement?.id);
  const r = {};
  r.original = await source();

  // Add a join from the disclosure, by the keyboard.
  r.addJoinOpenAtStart = await page.$eval('#add-join', (e) => e.open);
  await page.focus('#add-join summary');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('add-join').open, null, 'the add-join disclosure');
  await page.focus('#aj-name');
  await page.keyboard.type('both');
  await page.keyboard.press('Tab');
  await page.keyboard.type('merge_two');
  await page.keyboard.press('Tab');
  await page.focus('#aj-in-1');
  await page.keyboard.press('Space'); // charge
  await page.focus('#aj-in-2');
  await page.keyboard.press('Space'); // receipt
  await page.click('#aj-add');
  await until(() => document.getElementById('join-form'), null, 'the join form');
  await atRest();
  r.afterAdd = await source();
  r.focusAfterAdd = await focused();
  r.inspectorTitle = await text('#inspector h3');
  r.blockFieldOnJoin = await page.$('#f-block').then((e) => !!e);
  r.renameOnJoin = await page.$('#f-name').then((e) => !!e);
  r.inputs = await page.$$eval('#j-inputs li span', (l) => l.map((x) => x.textContent));
  r.removeInputDisabledAtTwo = await page.$eval('#j-in-rm-0', (b) => b.disabled);

  // Inputs: add one, move it up, remove one.
  await page.select('#j-in-new', 'validate');
  await page.click('#j-in-add');
  await until(() => document.querySelectorAll('#j-inputs li').length === 3, null, 'the third input');
  await atRest();
  r.afterAddInput = await source();
  await page.click('#j-in-up-2');
  await until(() => document.getElementById('source').value.includes('inputs: [charge, validate, receipt]'), null, 'the input to move');
  await atRest();
  r.afterMoveInput = await source();
  r.focusAfterMoveInput = await focused();
  await page.click('#j-in-rm-1');
  await until(() => document.querySelectorAll('#j-inputs li').length === 2, null, 'an input to go');
  await atRest();
  r.afterRemoveInput = await source();

  // The merge.
  await page.$eval('#j-merge', (e) => { e.value = ''; });
  await page.focus('#j-merge');
  await page.keyboard.type('merge_v2');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('merge: merge_v2'), null, 'the merge');
  await atRest();
  r.afterMerge = await source();

  // A gate on the join, with the same form a node has.
  await page.focus('#g-add');
  await page.keyboard.type('ops');
  await page.keyboard.press('Enter');
  await atRest();
  await page.click('#g-apply');
  await until(() => /approval:/.test(document.getElementById('source').value.split('join: both')[1] || ''), null, 'the gate on the join');
  await atRest();
  r.afterGate = await source();

  // An edge out of the join, from the same Connections form.
  await page.select('#c-edge-to', 'validate');
  await page.click('#c-edge-add');
  await until(() => document.getElementById('source').value.includes('edge: [both, validate]'), null, 'the edge out of the join');
  await atRest();
  r.afterJoinEdge = await source();
  r.joinOffersSwitch = await page.$('#c-sw-pred').then((e) => !!e);

  // A loop bound: receipt gets a switch whose arm goes back to charge.
  await page.click('#outline button[data-node="receipt"]');
  await until(() => document.getElementById('c-sw-pred'), null, 'the switch form');
  await atRest();
  await page.focus('#c-sw-pred');
  await page.keyboard.type('again');
  await page.select('#c-sw-to', 'charge');
  await page.click('#c-sw-add');
  await until(() => document.getElementById('c-arm-max-0'), null, 'the arm');
  await atRest();
  r.loopBefore = await page.$eval('#c-arm-max-0', (e) => e.value);
  await page.$eval('#c-arm-max-0', (e) => { e.value = ''; });
  await page.focus('#c-arm-max-0');
  await page.keyboard.type('3');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('loop_max: 3'), null, 'the loop bound');
  await atRest();
  r.afterLoop = await source();
  r.focusAfterLoop = await focused();
  await page.$eval('#c-arm-max-0', (e) => { e.value = ''; });
  await page.focus('#c-arm-max-0');
  await page.keyboard.type('0');
  await page.keyboard.press('Enter');
  await until(() => !document.getElementById('source').value.includes('loop_max'), null, 'the loop bound to go');
  await atRest();
  r.afterLoopCleared = await source();
  await page.$eval('#c-arm-max-0', (e) => { e.value = ''; });
  await page.focus('#c-arm-max-0');
  await page.keyboard.type('2.5');
  await page.keyboard.press('Enter');
  await until(() => /whole number/.test(document.getElementById('live').textContent), null, 'the refusal of a fraction');
  r.fractionLive = await text('#live');

  // Remove the join: the dialog lists what goes with it.
  await page.click('#outline button[data-node="both"]');
  await until(() => document.getElementById('j-remove'), null, 'the join form again');
  await atRest();
  await page.click('#j-remove');
  await until(() => document.getElementById('dialog').open, null, 'the dialog or the removal');
  r.dialogTitle = await text('#dialog-title');
  r.removeList = await page.$$eval('#remove-list li', (l) => l.map((x) => x.textContent));
  await page.click('#do-remove');
  await until(() => !document.getElementById('source').value.includes('join: both'), null, 'the join to go');
  await atRest();
  r.afterRemove = await source();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
