// undo.mjs: undo and redo cover every kind of structure edit, in real Chrome (flowedit/browser_test.go
// starts the server and runs this): a node added, renamed and moved, an edge, a switch, an arm, a loop
// bound, a join and a gate removal, each by its own control; then Undo walks back to the file's exact
// text one step at a time (the weakened banner follows: it is computed again by the server for each
// text), Redo walks forward to the last text, and after a save Undo still works and the page says the
// text differs from the file. It prints one JSON line.
import fs from 'node:fs';
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
  const pick = async (name) => { await page.click(`#outline button[data-node="${name}"]`); await atRest(); };
  const typed = async (sel, v) => { await page.$eval(sel, (e) => { e.value = ''; }); await page.focus(sel); await page.keyboard.type(v); };
  const r = {};
  const texts = [await source()];
  const banners = [await text('#weakened')];
  const step = async (what, fn, changedBy) => {
    await fn();
    await until((before) => document.getElementById('source').value !== before, texts.at(-1), what);
    await atRest();
    texts.push(await source());
    banners.push(await text('#weakened'));
  };

  await step('add a node', async () => {
    await page.focus('#an-name'); await page.keyboard.type('audit'); await page.keyboard.press('Tab'); await page.keyboard.type('audit_log'); await page.keyboard.press('Enter');
  });
  await step('rename it', async () => { await typed('#f-name', 'audit2'); await page.keyboard.press('Enter'); });
  await step('move it up', async () => { await page.click('#f-up'); });
  await step('an edge', async () => { await pick('receipt'); await page.select('#c-edge-to', 'audit2'); await page.click('#c-edge-add'); });
  await step('a switch', async () => { await pick('audit2'); await typed('#c-sw-pred', 'p'); await page.select('#c-sw-to', 'receipt'); await page.click('#c-sw-add'); });
  await step('an else (a loop needs an exit)', async () => { await page.select('#c-else', 'validate'); await page.click('#c-else-set'); });
  await step('a loop bound', async () => { await typed('#c-arm-max-0', '2'); await page.keyboard.press('Enter'); });
  await step('a join', async () => {
    await page.focus('#add-join summary'); await page.keyboard.press('Enter');
    await page.focus('#aj-name'); await page.keyboard.type('jn'); await page.keyboard.press('Tab'); await page.keyboard.type('mm');
    await page.focus('#aj-in-0'); await page.keyboard.press('Space');
    await page.click('#aj-add');
  });
  await step('the flow name', async () => { await typed('#fs-flow', 'refund2'); await page.keyboard.press('Enter'); });
  await step('the gate removed (weakens)', async () => { await pick('charge'); await page.click('#g-clear'); });
  r.bannerWhenWeakened = banners.at(-1);
  r.steps = texts.length - 1;

  // Undo every step: the text is the earlier text, byte for byte; the banner follows the text.
  r.undoMatches = [];
  r.bannerAfterFirstUndo = '';
  for (let i = texts.length - 2; i >= 0; i--) {
    await page.click('#undo');
    await until((want) => document.getElementById('source').value === want, texts[i], 'undo to step ' + i);
    await atRest();
    r.undoMatches.push((await source()) === texts[i] && (await text('#weakened')) === banners[i]);
    if (i === texts.length - 2) r.bannerAfterFirstUndo = await text('#weakened');
  }
  r.undoDisabledAtStart = await page.$eval('#undo', (b) => b.disabled);
  r.stateAtStart = await text('#state-tag');
  r.original = texts[0];
  // Redo every step.
  r.redoMatches = [];
  for (let i = 1; i < texts.length; i++) {
    await page.click('#redo');
    await until((want) => document.getElementById('source').value === want, texts[i], 'redo to step ' + i);
    await atRest();
    r.redoMatches.push((await source()) === texts[i] && (await text('#weakened')) === banners[i]);
  }
  r.redoDisabledAtEnd = await page.$eval('#redo', (b) => b.disabled);

  // Save, then Undo: the stack is kept, and the page says the text is not the file's.
  await page.click('#save');
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  const confirm = await page.$('#confirm-weaken');
  if (confirm) await page.click('#confirm-weaken');
  await page.click('#do-save');
  await until(() => !document.getElementById('dialog').open && document.getElementById('state-tag').textContent === 'saved', null, 'the save');
  r.disk = fs.readFileSync(env.FILE, 'utf8');
  r.undoEnabledAfterSave = await page.$eval('#undo', (b) => !b.disabled);
  await page.click('#undo');
  await until((want) => document.getElementById('source').value === want, texts.at(-2), 'undo after the save');
  await atRest();
  r.stateAfterUndoPastSave = await text('#state-tag');
  r.diskStill = fs.readFileSync(env.FILE, 'utf8') === r.disk;
  await page.click('#redo');
  await until((want) => document.getElementById('source').value === want, texts.at(-1), 'redo after the save');
  await atRest();
  r.stateAfterRedoToSaved = await text('#state-tag');
  r.last = texts.at(-1);
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
