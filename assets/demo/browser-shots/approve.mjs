// Drives the approve page in real Chrome against a real intake (../intake) and takes the screenshots.
import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const { CHROME_BIN, ENROL_DIR, THEME, OUT } = process.env;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const url = fs.readFileSync(path.join(ENROL_DIR, 'url'), 'utf8');
const profile = fs.mkdtempSync('/tmp/approve-shots-');
const browser = await puppeteer.launch({ executablePath: CHROME_BIN, headless: true, userDataDir: profile, args: ['--no-first-run'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: THEME }]);
  await page.setViewport({ width: 860, height: 900, deviceScaleFactor: 2 });
  await page.goto(url, { waitUntil: 'load' });
  await page.waitForSelector('#call-panel:not([hidden])');
  await page.waitForSelector('#create-key:not([hidden])');
  await page.click('#create-key');
  await page.waitForFunction(() => document.getElementById('key-public').textContent.startsWith('ed25519:'));
  fs.writeFileSync(path.join(ENROL_DIR, 'pub.tmp'), await page.$eval('#key-public', (e) => e.textContent));
  fs.renameSync(path.join(ENROL_DIR, 'pub.tmp'), path.join(ENROL_DIR, 'pub'));
  for (let i = 0; i < 100 && !fs.existsSync(path.join(ENROL_DIR, 'enrolled')); i++) await sleep(100);
  await page.type('#approver', 'ops');
  await page.waitForSelector('#approve:not([disabled])');
  await page.evaluate(() => document.activeElement.blur());
  const shot = async (name) => {
    await sleep(300);
    const h = await page.evaluate(() => document.documentElement.scrollHeight);
    await page.setViewport({ width: 860, height: h, deviceScaleFactor: 2 });
    await page.screenshot({ path: path.join(OUT, `${name}-${THEME}.png`) });
  };
  await shot('approve-page-waiting');
  await page.click('#approve');
  await page.waitForFunction(() => { const t = document.getElementById('result').textContent; return t !== '' && !t.startsWith('Signing'); });
  console.log(await page.$eval('#result', (e) => e.className + ': ' + e.textContent));
  await shot('approve-page-signed');
} finally {
  await browser.close();
}
