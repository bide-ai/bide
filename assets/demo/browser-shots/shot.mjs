// shot.mjs URL OUTNAME [width]: screenshot a page at 2x in both color schemes, full height.
import puppeteer from 'puppeteer-core';
const [url, name, width = '1200', sel] = process.argv.slice(2);
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run'] });
try {
  for (const theme of ['dark', 'light']) {
    const page = await browser.newPage();
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
    await page.setViewport({ width: +width, height: 900, deviceScaleFactor: 2 });
    await page.goto(url, { waitUntil: 'networkidle0' });
    const h = await page.evaluate(() => document.documentElement.scrollHeight);
    await page.setViewport({ width: +width, height: h, deviceScaleFactor: 2 });
    await page.screenshot({ path: `${process.env.OUT}/${name}-${theme}.png` });
    await page.close();
  }
} finally { await browser.close(); }
