// assertions.mjs: drives the assertions panel in real Chrome (flowedit/browser_test.go starts the
// server and runs this): add an assertion from a template and see its live verdict before adding it,
// edit it, see a duplicate and an approval form's verdict, remove a line and see the weakened banner.
// It prints one JSON line of what it saw.
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
  const live = () => text('#a-live');
  const r = {};
  r.rows = await page.$$eval('#assertions .assert code', (c) => c.map((x) => x.textContent));
  r.templates = await page.$$eval('#a-template option', (o) => o.map((x) => x.value));
  r.submitInitial = await text('#a-submit');

  // A form that would add an assertion the config breaks: the verdict is shown before it is added.
  await page.select('#a-template', 'no_side_effect');
  await until(() => document.getElementById('a-node'), null, 'the node field');
  await page.select('#a-node', 'validate');
  await until(() => document.getElementById('a-live')?.textContent.includes('no side effect after validate'), null, 'the live verdict');
  r.liveViolated = await live();
  r.sourceBeforeAdd = await source();

  // Add it, with the keyboard.
  await page.focus('#a-submit');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('no side effect after validate'), null, 'the assertion to be added');
  r.afterAdd = await source();
  r.rowsAfterAdd = await page.$$eval('#assertions .assert code', (c) => c.map((x) => x.textContent));
  r.verdictAfterAdd = await text('#vword');
  r.focusAfterAdd = await page.evaluate(() => document.activeElement?.id);

  // Edit the new line: its node to charge, which makes it hold.
  await page.click('#a-edit-2');
  await until(() => document.getElementById('a-cancel'), null, 'the edit form');
  r.editSubmit = await text('#a-submit');
  r.editTemplate = await page.$eval('#a-template', (e) => e.value);
  r.editNode = await page.$eval('#a-node', (e) => e.value);
  await page.select('#a-node', 'charge');
  await until(() => document.getElementById('a-live')?.textContent.includes('no side effect after charge'), null, 'the live verdict of the edit');
  r.liveEdit = await live();
  await page.click('#a-submit');
  await until(() => document.getElementById('source').value.includes('no side effect after charge'), null, 'the assertion to change');
  r.afterEdit = await source();
  r.focusAfterEdit = await page.evaluate(() => document.activeElement?.id);

  // A duplicate is said so, in the server's words.
  await page.select('#a-template', 'reachable');
  await until(() => document.getElementById('a-node'), null, 'the node field again');
  await page.select('#a-node', 'receipt');
  await until(() => document.getElementById('a-live')?.textContent.includes('already has'), null, 'the duplicate to be refused');
  r.liveDuplicate = await live();

  // The approval form: its fields, and a verdict for a line the gate does not meet.
  await page.select('#a-template', 'approval');
  await until(() => document.getElementById('a-k'), null, 'the approval fields');
  await page.select('#a-node', 'charge');
  await page.$eval('#a-k', (e) => { e.value = ''; });
  await page.focus('#a-k');
  await page.keyboard.type('2');
  await page.$eval('#a-n', (e) => { e.value = ''; });
  await page.focus('#a-n');
  await page.keyboard.type('3');
  await page.focus('#a-excluding');
  await page.keyboard.press('Space');
  await until(() => document.getElementById('a-live')?.textContent.includes('excluding the requester'), null, 'the approval verdict');
  r.liveApproval = await live();

  // Remove the first line (the gate assertion): weaker than the baseline.
  await page.click('#a-rm-0');
  await until(() => !document.getElementById('source').value.includes('every path to charge passes approval'), null, 'the assertion to be removed');
  r.afterRemove = await source();
  r.bannerAfterRemove = await text('#weakened');
  r.rowsAfterRemove = await page.$$eval('#assertions .assert code', (c) => c.map((x) => x.textContent));
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const l = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + l, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
