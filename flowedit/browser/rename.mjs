// rename.mjs: a rename of a gated node does not read as weakened, in real Chrome (flowedit/rename_browser_test.go
// starts the server and runs this). A gated node is renamed and its block changed (an edit the classifier
// alone cannot pair a renamed node across), so the banner is quiet only because the page carries the
// session's renames; the gate is then cleared (the banner shows), and Undo and Redo walk back and forth with
// the list each text had; a save of the renamed text asks for no confirmation and the page still shows it
// as not weaker. It prints one JSON line.
import fs from 'node:fs';
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
  const typed = async (sel, v) => { await page.$eval(sel, (e) => { e.value = ''; }); await page.focus(sel); await page.keyboard.type(v); };
  const seen = async () => { await atRest(); return { source: await source(), banner: (await text('#weakened')).trim(), baseline: await text('#baseline') }; };
  const states = [await seen()];
  const r = { original: states[0].source };

  stage = 'rename';
  await pick('charge');
  await typed('#f-name', 'charge_card');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: charge_card'), null, 'the rename');
  states.push(await seen());

  stage = 'block';
  await pick('charge_card');
  await typed('#f-block', 'charge_v2');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('block: charge_v2'), null, 'the block change');
  states.push(await seen());

  stage = 'clear the gate';
  await pick('charge_card');
  await page.click('#g-clear');
  await until(() => !document.getElementById('source').value.includes('approvers'), null, 'the gate to go');
  states.push(await seen());
  r.states = states;

  const go = async (button, want, what) => {
    await page.click(button);
    await until((w) => document.getElementById('source').value === w, want, what);
    return seen();
  };
  stage = 'undo';
  r.undone = [];
  r.undone.push(await go('#undo', states[2].source, 'undo of the gate'));
  r.undone.push(await go('#undo', states[1].source, 'undo of the block'));
  r.undone.push(await go('#undo', states[0].source, 'undo of the rename'));
  stage = 'redo';
  r.redone = [];
  r.redone.push(await go('#redo', states[1].source, 'redo of the rename'));
  r.redone.push(await go('#redo', states[2].source, 'redo of the block'));
  r.redone.push(await go('#redo', states[3].source, 'redo of the gate'));
  r.undoneAgain = await go('#undo', states[2].source, 'undo of the gate again');

  stage = 'save';
  await page.click('#save');
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  r.confirmBoxes = await page.$$eval('#dialog input[type=checkbox]', (b) => b.length);
  await page.click('#do-save');
  await until(() => !document.getElementById('dialog').open && document.getElementById('state-tag').textContent === 'saved', null, 'the save');
  r.afterSave = await seen();
  r.disk = fs.readFileSync(env.FILE, 'utf8');
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + ' (at ' + stage + '); the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
