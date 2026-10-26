// run-off.mjs: without bide edit -run the page shows no run panel and asks /api/run once, not on every
// edit (flowedit/run_browser_test.go). It prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
try {
  const page = await browser.newPage();
  let asked = 0;
  let askedAffected = 0;
  page.on('request', (r) => { if (r.url().endsWith('/api/run')) asked++; if (r.url().endsWith('/api/affected')) askedAffected++; });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 30000 });
  await atRest();
  for (let i = 0; i < 3; i++) {
    await page.$eval('#source', (e, i) => { e.value = e.value + '\n# edit ' + i; e.dispatchEvent(new Event('input', { bubbles: true })); }, i);
    await new Promise((r) => setTimeout(r, 600));
    await atRest();
  }
  out = { ok: true, hidden: await page.$eval('#run-panel', (e) => e.hidden), affectedHidden: await page.$eval('#affected-panel', (e) => e.hidden), lineHidden: await page.$eval('#run-line', (e) => e.hidden), asked, askedAffected };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e) };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
