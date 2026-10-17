// forms.mjs: drives the inspector's forms in real Chrome (flowedit/browser_test.go starts the server
// and runs this): set a node's safety, edit its approval gate (add an approver, raise the need, see
// the check's answer for a draft before applying it), remove the gate, undo, all by the controls a
// keyboard reaches. It prints one JSON line with what it saw: the source after each step, the banner,
// the draft feedback, and the browser's console, failed requests and off-origin requests.
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
  await page.setViewport({ width: 1400, height: 1200 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const source = () => page.$eval('#source', (e) => e.value);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const bannerText = () => page.$eval('#weakened', (e) => e.textContent);
  const r = {};
  r.original = await source();

  // Select the gated node by its outline button.
  await page.click('#outline button[data-node="charge"]');
  await until(() => document.getElementById('g-apply'), null, 'the gate form');
  r.gateButton = await text('#g-apply');
  r.needBefore = await page.$eval('#g-need', (e) => e.value);
  r.approversBefore = await page.$$eval('#g-approvers code', (c) => c.map((x) => x.textContent));
  r.safetyBefore = await page.$eval('#f-safety', (e) => e.value);
  r.excludeBefore = await page.$eval('#g-exclude', (e) => e.checked);
  r.timeoutBefore = await page.$eval('#g-timeout', (e) => e.value);

  // A draft that breaks the file's own assertion is said so before it is applied.
  await page.$eval('#g-need', (e) => { e.value = ''; });
  await page.focus('#g-need');
  await page.keyboard.type('1');
  await until(() => document.querySelector('#g-feedback')?.textContent.includes('is violated'), null, 'the check to reject need 1');
  r.assertionDraft = await text('#g-feedback');
  await page.$eval('#g-need', (e) => { e.value = ''; });
  await page.focus('#g-need');
  await page.keyboard.type('2');

  // Add an approver with the keyboard (type, Enter); the draft's check is shown before Apply.
  await page.focus('#g-add');
  await page.keyboard.type('audit');
  await page.keyboard.press('Enter');
  await until(() => document.querySelector('#g-feedback')?.textContent.includes('check accepts'), null, 'the check to accept the draft');
  r.kofn = await text('#g-kofn');
  r.sourceBeforeApply = await source();
  await page.focus('#g-apply');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('audit'), null, 'the gate to be applied');
  r.afterAddApprover = await source();
  r.bannerAfterAdd = await bannerText();
  r.stateAfterAdd = await text('#state-tag');
  r.focusAfterApply = await page.evaluate(() => document.activeElement?.id);
  r.undoEnabled = await page.$eval('#undo', (b) => !b.disabled);

  // Raise the need to 3: no longer weaker than the baseline.
  await page.$eval('#g-need', (e) => { e.value = ''; });
  await page.focus('#g-need');
  await page.keyboard.type('3');
  await until(() => document.querySelector('#g-kofn')?.textContent.startsWith('3 of 4'), null, 'the draft to say 3 of 4');
  await page.click('#g-apply');
  await until(() => document.getElementById('source').value.includes('need: 3'), null, 'need 3 to be applied');
  r.afterNeed = await source();
  r.bannerAfterNeed = await bannerText();

  // A draft the check rejects is said so before Apply, in the check's words.
  await page.$eval('#g-need', (e) => { e.value = ''; });
  await page.focus('#g-need');
  await page.keyboard.type('9');
  await until(() => document.querySelector('#g-feedback')?.textContent.includes('must be between'), null, 'the check to reject need 9');
  r.rejectedDraft = await text('#g-feedback');
  r.feedbackClass = await page.$eval('#g-feedback', (e) => e.className);
  r.sourceAfterRejectedDraft = await source();

  // Undo twice: the text is the original again.
  await page.click('#undo');
  await until(() => !document.getElementById('source').value.includes('need: 3'), null, 'undo');
  await page.click('#undo');
  await until(() => !document.getElementById('source').value.includes('audit'), null, 'the second undo');
  r.afterUndo = await source();
  r.redoEnabled = await page.$eval('#redo', (b) => !b.disabled);
  await page.click('#redo');
  await until(() => document.getElementById('source').value.includes('audit'), null, 'redo');
  r.afterRedo = await source();
  await page.click('#undo');
  await until(() => !document.getElementById('source').value.includes('audit'), null, 'undo after redo');

  // Safety, by the select.
  await page.click('#outline button[data-node="charge"]');
  await until(() => document.getElementById('f-safety'), null, 'the safety select');
  await page.select('#f-safety', 'idempotent');
  await until(() => document.getElementById('source').value.includes('safety: idempotent\n    approval'), null, 'safety to change');
  r.afterSafety = await source();
  r.focusAfterSafety = await page.evaluate(() => document.activeElement?.id);

  // Remove the gate: weaker than the baseline, said so.
  await page.click('#g-clear');
  await until(() => !document.getElementById('source').value.includes('approvers:'), null, 'the gate to be removed');
  r.afterClear = await source();
  r.bannerAfterClear = await bannerText();
  r.gateButtonAfterClear = await text('#g-apply');

  // A node with no gate in the config gets one.
  await page.click('#outline button[data-node="receipt"]');
  await until(() => document.getElementById('g-add'), null, 'the receipt form');
  await page.focus('#g-add');
  await page.keyboard.type('ops');
  await page.keyboard.press('Enter');
  await until(() => document.querySelector('#g-kofn')?.textContent.startsWith('1 of 1'), null, 'the draft to say 1 of 1');
  r.kofnOne = await text('#g-kofn');
  await page.keyboard.type('  audit2  ');
  await page.keyboard.press('Enter');
  await page.focus('#g-exclude');
  await page.keyboard.press('Space');
  await until(() => document.querySelector('#g-kofn')?.textContent.includes('never counting'), null, 'the draft to exclude the requester');
  await page.click('#g-apply');
  await until(() => document.getElementById('source').value.includes('name: receipt, block: receipt, safety: idempotent, approval'), null, 'receipt to be gated');
  r.afterGateReceipt = await source();

  // A refused operation (an empty block name): the server answers 422 {"error": reason}; the page shows
  // the reason under the form, announces it, and leaves the text as it was.
  await page.click('#outline button[data-node="validate"]');
  await until(() => document.getElementById('f-block'), null, 'the block field for a refusal');
  await page.$eval('#f-block', (e) => { e.value = ''; });
  const beforeRefusal = await source();
  await page.focus('#f-block');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('op-error')?.textContent.includes('cannot be empty'), null, 'the refusal reason');
  r.refusalShown = await text('#op-error');
  r.refusalLive = await text('#live');
  r.sourceAfterRefusal = (await source()) === beforeRefusal;

  // Block, by the text field.
  await page.click('#outline button[data-node="validate"]');
  await until(() => document.getElementById('f-block'), null, 'the block field');
  await page.$eval('#f-block', (e) => { e.value = ''; });
  await page.focus('#f-block');
  await page.keyboard.type('validate_v2');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('block: validate_v2'), null, 'the block to change');
  r.afterBlock = await source();
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
