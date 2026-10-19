// capture.mjs OUTFILE URL WIDTH HEIGHT THEME: one screenshot of a page at 1x, the way every shots*.sh
// script takes its pictures. The page is shown in the named color scheme (dark or light) with
// reduced motion, so the graph's flowing dashes and the live pulse stand still and the picture is the
// same every time; scrollbars are hidden; it waits for the network and the fonts to settle. A
// viewport of WIDTH x HEIGHT is photographed, not the whole page, so the height a script names is the
// height of the image. Chrome is CHROME_BIN. The caller (assets/demo/lib.sh) installs puppeteer-core
// from the lockfile.
import puppeteer from 'puppeteer-core';

const [out, url, width, height, theme] = process.argv.slice(2);
if (!out || !url || !width || !height || !['dark', 'light'].includes(theme)) {
  console.error('usage: capture.mjs OUTFILE URL WIDTH HEIGHT dark|light');
  process.exit(2);
}
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME_BIN, headless: true,
  args: ['--no-first-run', '--hide-scrollbars', '--force-device-scale-factor=1', '--font-render-hinting=none'],
});
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: +width, height: +height, deviceScaleFactor: 1 });
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: out });
} finally {
  await browser.close();
}
