// live-capture.mjs OUTFILE URL MS THEME: open a run page, keep it open MS milliseconds while its script
// follows the run (the caller drives the run meanwhile), then photograph it at 900 x 1500. It prints
// "loaded" once the page has loaded, so the caller can take the next step while the page is open.
import puppeteer from 'puppeteer-core';

const [out, url, ms, theme] = process.argv.slice(2);
if (!out || !url || !ms || !['dark', 'light'].includes(theme)) {
  console.error('usage: live-capture.mjs OUTFILE URL MS dark|light');
  process.exit(2);
}
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run', '--hide-scrollbars', '--force-device-scale-factor=1'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: 900, height: 1500, deviceScaleFactor: 1 });
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  console.log('loaded');
  await new Promise((r) => setTimeout(r, +ms));
  await page.screenshot({ path: out });
} finally {
  await browser.close();
}
