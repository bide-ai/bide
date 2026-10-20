// typed-race.mjs: what the save dialog asks, decided from the server's answer to Save and never from the page's
// own (possibly one edit old) view: CASE=race delays the check of the edit and clicks Save before it returns;
// CASE=kinds replaces the server's answer with items whose text says nothing and whose kinds say what they are;
// CASE=nokind leaves the kinds out. flowedit/browser_test.go starts the server and runs this. One JSON line.
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
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 15000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const mode = env.CASE;
  r.releaseReport = null;
  await page.setRequestInterception(true);
  let held = [];
  page.on('request', async (req) => {
    const url = req.url();
    if (req.isInterceptResolutionHandled()) return;
    if (mode === 'race' && url.endsWith('/api/report')) { held.push(req); return; } // held until the dialog is read
    if ((mode === 'kinds' || mode === 'nokind') && url.endsWith('/api/preview')) {
      const res = await fetch(url, { method: 'POST', headers: req.headers(), body: req.postData() });
      const body = await res.json();
      body.needs_confirmation = ['alpha', 'beta', 'gamma'];
      body.needs_kinds = mode === 'kinds' ? ['uncertified', 'weakens', 'uncompared'] : undefined;
      if (mode === 'nokind') body.needs_kinds = ['something-new'];
      await req.respond({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
      return;
    }
    await req.continue();
  });
  const edits = JSON.parse(env.EDITS || '[]') || [];
  await page.$eval('#source', (e, edits) => {
    let v = e.value;
    for (const [from, to] of edits) v = v.replace(from, to);
    e.value = edits.length ? v : v + '\n# a note\n';
    e.dispatchEvent(new Event('input', { bubbles: true }));
  }, edits);
  r.saveEnabled = await page.$eval('#save', (b) => !b.disabled);
  await page.click('#save'); // before the check of the edit has returned, in the race case
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  r.heldReports = held.length;
  r.groups = await page.$$eval('#dialog .confirm-group', (g) => g.map((x) => ({ title: x.querySelector('.vword').textContent, items: [...x.querySelectorAll('.vdetail')].map((d) => d.textContent).filter((t) => !/^\s*I confirm/.test(t)) })));
  r.viewVerdict = await page.$eval('#verdict .vword', (e) => e.textContent); // the page's own, one edit old, in the race case
  for (const q of held) await q.continue();
  held = [];
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, ...r, error: String(e && e.message || e), console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
