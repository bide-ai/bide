// run-session.mjs: the comparison with a run follows the editing session, in real Chrome
// (flowedit/run_browser_test.go starts the server). (1) A node renamed in the page is matched to the run's
// node, because the page sends the session's renames with the text. (2) An answer to older text never
// replaces the answer to newer text: the first /api/run request is held until the second has been answered.
// It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let stage = '';
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1400, height: 1800 });
  let hold = false;
  const held = [];
  let answered = 0;
  await page.setRequestInterception(true);
  page.on('request', (r) => {
    if (hold && r.method() === 'POST' && r.url().endsWith('/api/run')) { hold = false; held.push(r); return; }
    r.continue();
  });
  page.on('response', (r) => { if (r.url().endsWith('/api/run')) answered++; });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 30000 }, arg).catch(async () => {
    const seen = await page.evaluate(() => (document.getElementById('run-claim')?.textContent ?? '') + ' | ' + (document.getElementById('run-detail')?.textContent ?? '') + ' | ' + (document.getElementById('source')?.value ?? '')).catch(() => '');
    throw new Error('timed out waiting for ' + what + ' at ' + stage + '; the page said: ' + seen.slice(0, 1500));
  });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 30000 });
  const word = () => page.$eval('#run-word', (e) => e.textContent);
  const setSource = (v) => page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, v);
  page.on('dialog', (d) => d.accept()); // the unsaved-edits prompt of a reload

  stage = 'race';
  await until(() => document.getElementById('run-word'), null, 'the panel');
  await atRest();
  const first = await word();
  const original = await page.$eval('#source', (e) => e.value);
  hold = true;
  await setSource(original.replace('"block":"dbl"', '"block":"inc"')); // text 1: differs (its /api/run is held)
  for (let i = 0; i < 100 && held.length === 0; i++) await new Promise((r) => setTimeout(r, 50));
  if (held.length === 0) throw new Error('the first /api/run was not held');
  const before = answered;
  await setSource(original); // text 2: the run's own flow again
  for (let i = 0; i < 200 && answered === before; i++) await new Promise((r) => setTimeout(r, 50));
  if (answered === before) throw new Error('the second /api/run was not answered');
  const second = await word();
  await held[0].continue(); // the answer to the older text arrives last
  for (let i = 0; i < 200 && answered === before + 1; i++) await new Promise((r) => setTimeout(r, 50));
  await atRest();
  const afterStale = await word();

  stage = 'rename';
  await page.reload({ waitUntil: 'networkidle0', timeout: 30000 });
  await until(() => document.getElementById('run-word'), null, 'the panel');
  await atRest();
  await page.click('#outline button[data-node="b"]');
  await until(() => document.getElementById('f-name'), null, 'the inspector');
  await atRest();
  await page.$eval('#f-name', (e) => { e.value = ''; });
  await page.focus('#f-name');
  await page.keyboard.type('c');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('source').value.includes('"c"'), null, 'the rename');
  await until(() => document.getElementById('run-claim').textContent.includes('the renames made in this session'), null, 'the panel to name the rename');
  await atRest();
  const lists = await page.$$eval('#run-detail ul', (us) => us.map((u) => ({ label: document.getElementById(u.getAttribute('aria-labelledby'))?.textContent ?? '', items: [...u.children].map((l) => l.textContent) })));
  out = { ok: true, first, second, afterStale, lists };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e) };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
