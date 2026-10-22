// removeassert.mjs: removing a node takes the assertions that name it, in real Chrome
// (flowedit/remove_assert_browser_test.go starts the server and runs this). The removal dialog lists the
// assertion, the confirmed removal leaves none naming the node, the weakened banner names the removed
// assertion, the save dialog asks for the weakening confirmation and lists it, and Undo brings back the node
// and the assertion. It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
let stage = '';
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
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 30000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 30000 });
  const pick = async (name) => { await atRest(); await page.click(`#outline button[data-node="${name}"]`); await until(() => document.getElementById('f-name'), null, 'the inspector'); await atRest(); };
  const r = { original: await source() };

  stage = 'remove';
  await pick('audit');
  await page.click('#f-remove');
  await until(() => document.getElementById('dialog').open && document.getElementById('remove-list'), null, 'the removal dialog');
  await atRest();
  r.removeList = await page.$$eval('#remove-list li', (l) => l.map((x) => x.textContent));
  await page.click('#do-remove');
  await until(() => !document.getElementById('source').value.includes('name: audit'), null, 'the removal');
  await atRest();
  r.afterRemove = await source();
  r.banner = await text('#weakened');

  stage = 'save dialog';
  await page.click('#save');
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  r.saveDialog = await text('#dialog-body');
  r.confirmBoxes = await page.$$eval('#dialog input[type=checkbox]', (b) => b.length);
  await page.keyboard.press('Escape');
  await until(() => !document.getElementById('dialog').open, null, 'the dialog to close');

  stage = 'undo';
  await page.click('#undo');
  await until((w) => document.getElementById('source').value === w, r.original, 'undo of the removal');
  await atRest();
  r.afterUndo = await source();
  r.bannerAfterUndo = (await text('#weakened')).trim();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + ' (at ' + stage + '); the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
