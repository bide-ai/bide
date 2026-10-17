// races.mjs: the page when an answer is late or fails (browser/forms.mjs covers the normal path). With
// request interception it delays /api/apply and aborts /api/report, and checks that the author's typing is
// never overwritten, that a failed undo loses no history, and that a number the field shows is the
// number that is sent. It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const console_ = [];
const failed = [];
const pageErrors = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => pageErrors.push(e.message));
  page.on('requestfailed', (r) => failed.push(`${new URL(r.url()).pathname} ${r.failure()?.errorText}`));
  const mode = { delayApply: 0, abortReport: false };
  await page.setRequestInterception(true);
  page.on('request', (req) => {
    const path = new URL(req.url()).pathname;
    if (path === '/api/apply' && mode.delayApply) {
      const ms = mode.delayApply;
      setTimeout(() => req.continue().catch(() => {}), ms);
    } else if (path === '/api/report' && mode.abortReport) {
      req.abort('failed');
    } else {
      req.continue();
    }
  });
  await page.setViewport({ width: 1400, height: 1200 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const type = (v) => page.$eval('#source', (e, x) => { e.value = x; e.dispatchEvent(new Event('input', { bubbles: true })); }, v);
  const r = {};
  r.original = await source();
  await page.click('#outline button[data-node="charge"]');
  await until(() => document.getElementById('f-safety'), null, 'the form');

  // 1. Typing while an operation's answer is on its way is not overwritten.
  mode.delayApply = 1200;
  await page.select('#f-safety', 'idempotent');
  await new Promise((res) => setTimeout(res, 200));
  await type(r.original + '#TYPED MEANWHILE\n');
  await until(() => document.getElementById('op-error')?.textContent.includes('text changed'), null, 'the late answer to be refused');
  r.lateSource = await source();
  r.lateMessage = await text('#op-error');
  r.lateLive = await text('#live');
  mode.delayApply = 0;
  await type(r.original); // back to the original text

  // 2. A failed undo loses no history: two operations, an aborted report, then a good undo goes back one step.
  await page.click('#outline button[data-node="validate"]');
  await until(() => document.getElementById('f-safety'), null, 'the validate form');
  await page.select('#f-safety', 'idempotent');
  await until(() => document.getElementById('source').value.includes('name: validate, block: validate, safety: idempotent'), null, 'first operation');
  const afterFirst = await source();
  await page.select('#f-safety', 'side_effect');
  await until(() => document.getElementById('source').value.includes('safety: side_effect}'), null, 'second operation');
  const afterSecond = await source();
  mode.abortReport = true;
  await page.click('#undo');
  await until(() => document.getElementById('live')?.textContent.startsWith('Not undone'), null, 'the failure to be announced');
  r.failedUndoLive = await text('#live');
  r.failedUndoSource = (await source()) === afterSecond;
  r.undoStillEnabled = await page.$eval('#undo', (b) => !b.disabled);
  r.redoStillDisabled = await page.$eval('#redo', (b) => b.disabled);
  mode.abortReport = false;
  await page.click('#undo');
  await until(() => document.getElementById('live')?.textContent === 'Undone', null, 'the good undo');
  r.undoneOneStep = (await source()) === afterFirst;

  // 3. A number the field shows is the number sent: "1e3" is refused, not sent as 1.
  await page.click('#outline button[data-node="charge"]');
  await until(() => document.getElementById('g-need'), null, 'the gate form');
  const before = await source();
  for (const bad of ['1e3', '2.5', '', '-1']) {
    await page.$eval('#g-need', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, bad);
    r['needBad_' + bad] = {
      msg: await text('#g-feedback'),
      applyDisabled: await page.$eval('#g-apply', (b) => b.disabled),
      invalid: await page.$eval('#g-need', (e) => e.getAttribute('aria-invalid')),
    };
  }
  await page.$eval('#g-need', (e) => { e.value = '3'; e.dispatchEvent(new Event('input', { bubbles: true })); });
  r.needGoodApplyEnabled = await page.$eval('#g-apply', (b) => !b.disabled);
  r.sourceUnchanged = (await source()) === before;
  out = { ok: true, ...r, console: console_, failed, pageErrors };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, pageErrors };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
