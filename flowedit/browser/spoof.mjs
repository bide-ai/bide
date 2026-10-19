// spoof.mjs: a node name with characters that do not print (a hand-written file can have one) is
// shown escaped in the outline, the inspector and the drawing, never rendered. Prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const console_ = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.waitForSelector('#outline button');
  const r = {};
  r.outline = await page.$$eval('#outline button .nm', (b) => b.map((x) => x.textContent));
  r.graph = await page.$$eval('#canvas text', (t) => t.map((x) => x.textContent));
  await page.click('#outline button[data-node="ch‮gra"]');
  await page.waitForSelector('#f-name-escaped');
  r.title = await page.$eval('#inspector h3', (e) => e.textContent);
  r.warning = await page.$eval('#f-name-escaped', (e) => e.textContent);
  r.nameField = await page.$eval('#f-name', (e) => e.value);
  out = { ok: true, ...r, console: console_ };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), console: console_ };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
