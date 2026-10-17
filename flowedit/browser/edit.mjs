// edit.mjs: drives the editing and saving of bide edit's page in real Chrome (flowedit/browser_test.go
// starts the server and runs this). It types a weakening edit into the source, sees the banner, saves
// through the dialog (with the confirmation a weakened save needs, by keyboard), then changes the file
// behind the page's back and tries to save over it. It prints one JSON line of what it saw and what
// the browser did.
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
  await page.setViewport({ width: 1400, height: 1000 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });

  const r = {};
  r.stateBefore = await text('#state-tag');
  r.saveDisabledBefore = await page.$eval('#save', (b) => b.disabled);

  // Type: lower the need in the source.
  const src = await page.$eval('#source', (e) => e.value);
  const edited = src.replace('need: 2', 'need: 1');
  await page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, edited);
  await until(() => document.querySelector('#weakened section[role="alert"]'), null, 'the weakened banner');
  r.banner = await text('#weakened');
  r.bannerControls = await page.$$eval('#weakened button, #weakened a, #weakened [tabindex]', (e) => e.length);
  // A banner that has not changed is not drawn again: an alert drawn again is spoken again.
  await page.evaluate(() => { window.__mutations = 0; new MutationObserver((m) => { window.__mutations += m.length; }).observe(document.getElementById('weakened'), { childList: true, subtree: true }); });
  await page.$eval('#source', (e, v) => { e.value = v + '# a comment\n'; e.dispatchEvent(new Event('input', { bubbles: true })); }, edited);
  await new Promise((res) => setTimeout(res, 900));
  r.bannerMutationsWhileTyping = await page.evaluate(() => window.__mutations);
  await page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, edited);
  await new Promise((res) => setTimeout(res, 900));
  r.stateDirty = await text('#state-tag');
  r.saveEnabled = await page.$eval('#save', (b) => !b.disabled);
  r.verdictAfterEdit = await text('#vword');

  // Save, with the keyboard: focus the Save button and press Enter, then Tab to the checkbox.
  await page.$eval('#save', (b) => b.focus());
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  r.dialogTitle = await text('#dialog-title');
  r.dialogText = await text('#dialog-body');
  r.diffLines = await page.$$eval('#dialog .diff span', (s) => s.map((x) => x.textContent));
  r.doSaveDisabledBefore = await page.$eval('#do-save', (b) => b.disabled);
  r.focusInDialog = await page.evaluate(() => document.activeElement?.id);
  await page.keyboard.press('Space'); // the focused checkbox
  r.doSaveDisabledAfter = await page.$eval('#do-save', (b) => b.disabled);
  await page.$eval('#do-save', (b) => b.focus());
  await page.keyboard.press('Enter');
  await until(() => !document.getElementById('dialog').open && document.getElementById('state-tag').textContent === 'saved', null, 'the save to finish');
  r.stateAfterSave = await text('#state-tag');
  r.disk1 = fs.readFileSync(env.FILE, 'utf8');
  r.focusAfterClose = await page.evaluate(() => document.activeElement?.id);
  r.liveAfterSave = await text('#live');

  // Another writer changes the file; the page does not know.
  fs.writeFileSync(env.FILE, r.disk1.replace('flow: refund', 'flow: refund-other'));
  const src2 = await page.$eval('#source', (e) => e.value);
  await page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, src2 + '# a late note\n');
  await page.$eval('#save', (b) => b.focus());
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('dialog').open && document.getElementById('dialog-title').textContent.includes('changed on disk'), null, 'the conflict dialog');
  r.conflictText = await text('#dialog-body');
  r.conflictButtons = await page.$$eval('#dialog-actions button', (b) => b.map((x) => x.textContent));
  r.disk2 = fs.readFileSync(env.FILE, 'utf8');
  r.conflictDiffLines = await page.$$eval('#dialog .diff span', (s) => s.map((x) => x.textContent));
  // Reload discards the edit and shows the file as it is.
  const reload = (await page.$$('#dialog-actions button'))[1];
  await reload.click();
  await until(() => !document.getElementById('dialog').open, null, 'the dialog to close');
  r.sourceAfterReload = await page.$eval('#source', (e) => e.value);
  r.stateAfterReload = await text('#state-tag');
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
