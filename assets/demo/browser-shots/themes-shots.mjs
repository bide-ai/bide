// themes-shots.mjs URL OUTDIR: the themes guide's pictures: one bide view page (a flow config's risk map) in each
// theme, 1200 x 900 at 1x: themes-light.png, themes-dark.png, themes-high-contrast.png and themes-acme-teal.png (the
// custom theme of acme-teal.json, which the server was started with -theme). The theme is chosen the way the page's
// Theme button chooses it, by the data-theme attribute on <html>, so each picture is the page as a reader who
// picked that theme sees it. Reduced motion, so the picture is the same every time.
import puppeteer from 'puppeteer-core';

const [url, outDir] = process.argv.slice(2);
if (!url || !outDir) {
  console.error('usage: themes-shots.mjs URL OUTDIR');
  process.exit(2);
}
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run', '--hide-scrollbars', '--force-device-scale-factor=1', '--font-render-hinting=none'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: 1200, height: 900, deviceScaleFactor: 1 });
  const resp = await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  if (!resp || !resp.ok()) throw new Error(`${url} answered ${resp ? resp.status() : 'nothing'}`);
  await page.waitForSelector('main', { timeout: 15000 });
  await page.evaluate(() => document.fonts.ready);
  for (const name of ['light', 'dark', 'high-contrast', 'acme-teal']) {
    await page.evaluate((n) => document.documentElement.setAttribute('data-theme', n), name);
    // The page must know the theme (the custom one only if the server loaded it): its token is the proof.
    const bg = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--bg').trim());
    if (!bg) throw new Error(`the page has no colors for the theme ${name}`);
    await page.evaluate(() => { document.activeElement && document.activeElement.blur(); window.scrollTo(0, 0); });
    await new Promise((r) => setTimeout(r, 300));
    await page.screenshot({ path: `${outDir}/themes-${name}.png` });
  }
} finally {
  await browser.close();
}
