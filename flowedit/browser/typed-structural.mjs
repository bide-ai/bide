// typed-structural.mjs: the structural editor shows nothing of typed mode, and a text that has errors
// against the program's blocks shows the NOT CERTIFIED banner beside (not in place of) the weakened one.
// flowedit/browser_test.go starts the server and runs this. It prints one JSON line.
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
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const r = {};
  r.typedTagHidden = await page.$eval('#typed-tag', (e) => e.hidden);
  r.blocksHidden = await page.$eval('#blocks', (e) => e.hidden);
  r.skipHidden = await page.$eval('a[data-skip="blocks"]', (a) => a.hidden);
  r.typedInfo = await page.$eval('#typed-info', (e) => e.children.length);
  r.banners = await page.$$eval('#weakened section', (s) => s.map((x) => ({ role: x.getAttribute('role'), word: x.querySelector('.vword').textContent, line: x.querySelector('.vline').textContent, cls: x.className })));
  r.baseline = await text('#baseline');
  out = { ok: true, ...r, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
