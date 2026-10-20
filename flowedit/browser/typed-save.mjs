// typed-save.mjs: the save dialog of a text that cannot be certified against the program's blocks, and an existing edge
// whose types differ, in real Chrome (flowedit/browser_test.go starts the server and runs this). It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
const r = {};
try {
  page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (q) => failed.push(`${q.method()} ${q.url()} ${q.failure()?.errorText}`));
  page.on('request', (q) => { if (!q.url().startsWith(origin) && !q.url().startsWith('data:')) offOrigin.push(q.url()); });
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  // An existing edge to a node of another type is marked in the inspector, as a new target is.
  await page.click('#outline button[data-node="validate"]');
  await until(() => document.getElementById('connections'), null, 'the connections form');
  await atRest();
  r.existingEdges = await page.$$eval('#c-outs li span', (s) => s.map((x) => x.textContent));
  // The edits the case makes (EDITS: [[from, to], ...], a comment when empty), then Save.
  const edits = JSON.parse(env.EDITS || '[]') || [];
  await page.$eval('#source', (e, edits) => {
    let v = e.value;
    for (const [from, to] of edits) v = v.replace(from, to);
    e.value = edits.length ? v : v + '\n# a note\n';
    e.dispatchEvent(new Event('input', { bubbles: true }));
  }, edits);
  await atRest();
  await page.click('#save');
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  r.groups = await page.$$eval('#dialog .confirm-group', (g) => g.map((x) => ({ title: x.querySelector('.vword').textContent, label: x.querySelector('label').textContent, id: x.querySelector('input').id, labelled: x.getAttribute('aria-label') })));
  r.dialogText = await text('#dialog');
  r.saveDisabledAtStart = await page.$eval('#do-save', (b) => b.disabled);
  r.afterEach = [];
  for (const g of r.groups) {
    await page.click('#' + g.id);
    r.afterEach.push(await page.$eval('#do-save', (b) => b.disabled));
  }
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, ...r, error: String(e && e.message || e), console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
