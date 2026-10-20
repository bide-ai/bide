// structure.mjs: drives the structure controls in real Chrome (flowedit/browser_test.go starts the
// server and runs this): add a node, rename it, remove it, remove a wired node through the dialog that
// lists what else goes, make a node the entry, and a rename of a gated node reading as weakened, all by
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
let stage = "";
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
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 30000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  // The page says when it is at rest (data-busy on <html> counts its requests in flight and the pauses it
  // waits out before it asks the server). A read made before that sees a half-drawn state: the source
  // text changes before the outline, the inspector and the focus follow it, and a click made then can
  // land on an element the next redraw replaces. Every read and every click below waits for rest first.
  const rest = (what) => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 30000 }).catch(() => { throw new Error('timed out waiting for the page to be at rest ' + what); });
  const focused = () => page.evaluate(() => document.activeElement?.id);
  const bannerText = () => page.$eval('#weakened', (e) => e.textContent);
  const replaceValue = async (sel, value) => { await page.$eval(sel, (e) => { e.value = ''; }); await page.focus(sel); await page.keyboard.type(value); };
  const r = {};

  await rest('after the page opened');
  r.original = await source();

  // Add a node with the keyboard: name, Tab, block, Enter.
  await page.focus('#an-name');
  await page.keyboard.type('audit');
  await page.keyboard.press('Tab');
  await page.keyboard.type('audit_log');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: audit,'), null, 'the node to be added');
  await rest('after the node to be added');
  r.afterAdd = await source();
  r.focusAfterAdd = await focused();
  r.inspectorAfterAdd = await text('#inspector h3');
  r.outlineAfterAdd = await page.$$eval('#outline button .nm', (b) => b.map((x) => x.textContent));
  r.live = await text('#live');

  // A name already used is refused, in words, and the text stays.
  const beforeDup = await source();
  await page.focus('#an-name');
  await page.keyboard.type('charge');
  await page.keyboard.press('Tab');
  await page.keyboard.type('x');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('an-error')?.textContent.includes('already there'), null, 'the duplicate to be refused');
  await rest('after the duplicate to be refused');
  r.dupError = await text('#an-error');
  r.dupTextSame = (await source()) === beforeDup;
  await replaceValue('#an-name', '');
  await replaceValue('#an-block', '');

  // Rename it.
  await replaceValue('#f-name', 'audit2');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: audit2,'), null, 'the rename');
  await rest('after the rename');
  r.afterRename = await source();
  r.focusAfterRename = await focused();
  r.inspectorAfterRename = await text('#inspector h3');

  // Remove it: nothing else goes, so there is no dialog.
  stage = "await page.click('#f-remove');";
  await page.click('#f-remove');
  await until(() => !document.getElementById('source').value.includes('audit2'), null, 'the removal');
  await rest('after the removal');
  r.afterRemoveLone = await source();
  r.focusAfterRemove = await focused();
  r.dialogOpenAfterLone = await page.$eval('#dialog', (d) => d.open);

  // Remove a wired node: the dialog lists what else goes; Cancel leaves the text, Remove takes it all.
  stage = 'await page.click(\'#outline button[data-node="receipt"]\');';
  await page.click('#outline button[data-node="receipt"]');
  await until(() => document.getElementById('f-remove'), null, 'the structure form');
  await rest('after the structure form');
  stage = "await page.click('#f-remove');";
  await page.click('#f-remove');
  await until(() => document.getElementById('dialog').open && document.getElementById('remove-list'), null, 'the dialog');
  await rest('after the dialog');
  r.removeList = await page.$$eval('#remove-list li', (l) => l.map((x) => x.textContent));
  r.dialogTitle = await text('#dialog-title');
  stage = "await page.click('#dialog .dialog-actions button:not(.primary)');";
  await page.click('#dialog .dialog-actions button:not(.primary)');
  await rest('after Cancel');
  r.afterCancel = await source();
  r.focusAfterCancel = await focused();
  stage = "await page.click('#f-remove');";
  await page.click('#f-remove');
  await until(() => document.getElementById('dialog').open && document.getElementById('do-remove'), null, 'the dialog again');
  await rest('after the dialog again');
  stage = "await page.click('#do-remove');";
  await page.click('#do-remove');
  await until(() => !document.getElementById('source').value.includes('name: receipt'), null, 'the removal of receipt');
  await rest('after the removal of receipt');
  r.afterRemoveWired = await source();
  r.stateAfterRemove = await text('#state-tag');
  stage = "await page.click('#undo');";
  await page.click('#undo');
  await until(() => document.getElementById('source').value.includes('name: receipt'), null, 'undo');
  await rest('after undo');
  r.afterUndo = await source();

  // The entry: make charge the entry; the entry node cannot then be removed.
  stage = 'await page.click(\'#outline button[data-node="charge"]\');';
  await page.click('#outline button[data-node="charge"]');
  await until(() => document.getElementById('f-entry'), null, 'the entry button');
  await rest('after the entry button');
  r.entryButton = await text('#f-entry');
  stage = "await page.click('#f-entry');";
  await page.click('#f-entry');
  await until(() => document.getElementById('source').value.includes('entry: charge'), null, 'the entry to change');
  await rest('after the entry to change');
  r.afterEntry = await source();
  stage = "await page.click('#f-remove');";
  await page.click('#f-remove');
  await until(() => document.getElementById('op-error')?.textContent.includes('is the entry node'), null, 'the refusal');
  await rest('after the refusal');
  r.entryRefusal = await text('#op-error');
  r.dialogOpenAfterRefusal = await page.$eval('#dialog', (d) => d.open);

  // Renaming a gated node reads as removed and added: the banner says so.
  await replaceValue('#f-name', 'charge2');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('name: charge2'), null, 'the gated rename');
  await rest('after the gated rename');
  r.afterGatedRename = await source();
  await until(() => document.getElementById('weakened')?.textContent.includes('WEAKENED'), null, 'the banner');
  await rest('after the banner');
  r.bannerAfterGatedRename = await bannerText();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + ' (after ' + (typeof stage !== 'undefined' ? stage : '') + '); the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
