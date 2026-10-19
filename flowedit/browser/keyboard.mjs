// keyboard.mjs: the whole path, with the keyboard alone: from a page just opened, select a node in the
// outline with the arrow keys, set its safety, add an approver and raise the deadline in the gate
// form, add an assertion, undo one step, and save through the dialog with the confirmation a weakening
// needs. The pointer is never used (no click, no mouse move): every focus is reached by Tab, Shift+Tab
// or an arrow key, and the script fails if it needs more than a bounded number of Tab presses to
// reach a control. It prints one JSON line.
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
const tabs = {};
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
  const active = () => page.evaluate(() => { const a = document.activeElement; return a ? (a.id || a.getAttribute('data-node') || a.tagName) : ''; });
  // tabTo presses Tab (or Shift+Tab) until the focused element matches sel, at most max times, and
  // records how many it took.
  const tabTo = async (name, sel, { back = false, max = 40 } = {}) => {
    for (let i = 1; i <= max; i++) {
      if (back) await page.keyboard.down('Shift');
      await page.keyboard.press('Tab');
      if (back) await page.keyboard.up('Shift');
      if (await page.evaluate((s) => document.activeElement?.matches(s), sel)) { tabs[name] = i; return; }
    }
    throw new Error(`more than ${max} Tab presses to reach ${name} (${sel}); focus is on ${await active()}`);
  };
  const r = {};

  // The outline: one Tab stop, then the arrow keys; Enter selects.
  await tabTo('the outline', '#outline button');
  r.outlineFirst = await active();
  await page.keyboard.press('ArrowDown');
  r.afterArrow = await active();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowUp');
  r.afterArrowBack = await active();
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('g-apply'), null, 'the gate form');
  r.inspectorTitle = await text('#inspector h3');
  r.outlineTabStops = await page.$$eval('#outline button', (b) => b.filter((x) => x.tabIndex === 0).length);

  // The safety select: Tab to it, type to choose.
  await tabTo('the safety select', '#f-safety');
  await page.keyboard.type('i'); // idempotent
  await until(() => document.getElementById('source').value.includes('safety: idempotent\n    approval'), null, 'the safety to change');
  r.focusAfterSafety = await active();

  // The gate form: Tab to the approver field, add one, Tab to Apply.
  await tabTo('the approver field', '#g-add');
  await page.keyboard.type('audit');
  await page.keyboard.press('Enter');
  await tabTo('the deadline field', '#g-timeout', { max: 12 });
  await page.keyboard.down('Control'); await page.keyboard.press('KeyA'); await page.keyboard.up('Control');
  await page.keyboard.type('72h');
  await until(() => document.querySelector('#g-feedback')?.textContent.includes('check accepts'), null, 'the check to accept the draft');
  await tabTo('Apply gate', '#g-apply', { max: 6 });
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('audit'), null, 'the gate to be applied');
  r.afterGate = await source();
  r.focusAfterGate = await active();

  // An assertion from its form: Tab to the template, choose, Tab to the node, Tab to Add.
  await tabTo('the assertion template', '#a-template');
  // Skip links (WCAG 2.4.1): from this form, back to the region's own skip link, and Enter takes the
  // focus to the toolbar's first control, Save. The key presses it takes are bounded at 5 by the Go test.
  let presses = 0;
  for (; presses < 20;) {
    presses++;
    await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    if (await page.evaluate(() => document.activeElement?.matches('section[aria-labelledby="h-assert"] a[data-skip="toolbar"]'))) break;
  }
  r.skipLinkLabel = await page.evaluate(() => document.activeElement?.textContent);
  r.skipVisible = await page.evaluate(() => { const b = document.activeElement.getBoundingClientRect(); return b.width > 40 && b.height > 12; });
  await page.keyboard.press('Enter');
  presses++;
  tabs['Skip to Save'] = presses;
  r.focusAfterSkip = await active();
  // And back to the assertions by the skip link at the top of the page (one Shift+Tab from Save), Enter,
  // then Tab to the form: no walk over the whole page.
  await tabTo('the skip link to assertions', 'a[data-skip="assertions"]', { back: true, max: 4 });
  await page.keyboard.press('Enter');
  await tabTo('the assertion template again', '#a-template', { max: 12 });
  await page.keyboard.type('u'); // typeahead: "Unreachable: ..."
  await until(() => document.getElementById('a-node'), null, 'the node field');
  await tabTo('the assertion node', '#a-node', { max: 3 });
  await page.keyboard.type('v'); // validate
  await tabTo('Add assertion', '#a-submit', { max: 4 });
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('validate unreachable'), null, 'the assertion to be added');
  r.focusAfterAssertion = await active();

  // Undo, by keyboard, then redo the same step so the file keeps the assertion.
  await tabTo('Undo', '#undo', { back: true, max: 120 });
  await page.keyboard.press('Enter');
  await until(() => !document.getElementById('source').value.includes('validate unreachable'), null, 'undo');
  await tabTo('Redo', '#redo', { max: 3 });
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('validate unreachable'), null, 'redo');

  // Save: the dialog traps the focus; the confirmation is a checkbox reached by Tab.
  await tabTo('Save', '#save', { back: true, max: 120 });
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  r.dialogFocus = await active();
  r.dialogTitle = await text('#dialog-title');
  r.weakensShown = (await text('#dialog-body')).includes('THIS WEAKENS THE POLICY');
  // Escape closes it and gives the focus back to Save.
  await page.keyboard.press('Escape');
  await until(() => !document.getElementById('dialog').open, null, 'Escape to close the dialog');
  r.focusAfterEscape = await active();
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('dialog').open, null, 'the dialog to open again');
  // Tab never reaches the page behind the dialog: the focus is in the dialog, or has left the
  // document for the browser's own controls (document.body), as it does from a modal dialog.
  const inside = [];
  for (let i = 0; i < 12; i++) { await page.keyboard.press('Tab'); inside.push(await page.evaluate(() => (document.activeElement?.closest('dialog') || document.activeElement === document.body) ? 'in' : 'out')); }
  r.tabStaysInDialog = inside.every((x) => x === 'in');
  await tabTo('the confirmation', '#confirm-weaken', { max: 8 });
  await page.keyboard.press('Space');
  await tabTo('the dialog Save', '#do-save', { max: 4 });
  await page.keyboard.press('Enter');
  await until(() => !document.getElementById('dialog').open && document.getElementById('state-tag').textContent === 'saved', null, 'the save');
  r.stateAfterSave = await text('#state-tag');
  r.focusAfterSave = await active();
  r.disk = fs.readFileSync(env.FILE, 'utf8');
  out = { ok: true, ...r, tabs, console: console_, failed, offOrigin };
} catch (e) {
  const live = await page?.$eval('#live', (x) => x.textContent).catch(() => '?');
  const tail = await page?.$eval('#source', (x) => x.value.slice(-160)).catch(() => '?');
  out = { ok: false, error: String(e && e.message || e) + '; the live region says: ' + live + '; the source ends: ' + JSON.stringify(tail), tabs, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
