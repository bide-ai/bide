// share.mjs URL OUTFILE THEME: what the run page's "Share this run" button saves, as a file. The page
// draws the card itself (an SVG on a canvas, saved as a download), so this opens a run page, presses the
// button and keeps the PNG the browser downloads: nothing is composed here.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const [url, out, theme] = process.argv.slice(2);
if (!url || !out || !['dark', 'light'].includes(theme)) {
  console.error('usage: share.mjs URL OUTFILE dark|light');
  process.exit(2);
}
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bide-share-'));
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run', '--force-device-scale-factor=1'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: 1200, height: 900, deviceScaleFactor: 1 });
  const client = await page.createCDPSession();
  await client.send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: dir });
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  await page.evaluate(() => document.fonts.ready);
  await page.click('.verdict .share');
  let file = null;
  for (let i = 0; i < 100 && !file; i++) {
    await new Promise((r) => setTimeout(r, 100));
    file = fs.readdirSync(dir).find((f) => f.endsWith('.png'));
  }
  if (!file) throw new Error('the page saved no image');
  fs.copyFileSync(path.join(dir, file), out);
} finally {
  await browser.close();
  fs.rmSync(dir, { recursive: true, force: true });
}
