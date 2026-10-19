// edit-shots.mjs URL OUTDIR THEME: the two pictures of bide edit's guide, from the real page. It opens
// the editor on a flow committed to git, removes the separation-of-duties line from the source (the
// edit the gate form's "exclude the requester" box makes) with the charge node selected, waits for the weakened banner and
// photographs the page (flow-edit-weakened.png), then presses Save and photographs the dialog with the
// diff and the confirmation box (flow-edit-save.png). Both are 1280 x 900.
import puppeteer from 'puppeteer-core';

const [url, outDir, theme] = process.argv.slice(2);
if (!url || !outDir || !['dark', 'light'].includes(theme)) {
  console.error('usage: edit-shots.mjs URL OUTDIR dark|light');
  process.exit(2);
}
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run', '--hide-scrollbars', '--force-device-scale-factor=1'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: 1280, height: 900, deviceScaleFactor: 1 });
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  await page.evaluate(() => document.fonts.ready);
  const until = (fn, what) => page.waitForFunction(fn, { timeout: 15000 }).catch(() => { throw new Error('timed out waiting for ' + what); });
  // The charge node selected, as it is when its gate form is in use: the inspector shows its forms.
  await page.$eval('#outline button[data-node="charge"]', (b) => b.click());
  await until(() => document.getElementById('g-apply'), 'the charge node\'s forms');
  const src = await page.$eval('#source', (e) => e.value);
  const edited = src.replace(/^\s*exclude_principal: true\n/m, '').replace(/, exclude_principal: true/, '');
  if (edited === src) throw new Error('the flow has no exclude_principal line to remove');
  await page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, edited);
  await until(() => document.querySelector('#weakened section[role="alert"]'), 'the weakened banner');
  await page.evaluate(() => document.activeElement && document.activeElement.blur());
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: `${outDir}/flow-edit-weakened.png` });
  await page.click('#save');
  await until(() => document.getElementById('dialog').open, 'the save dialog');
  await page.screenshot({ path: `${outDir}/flow-edit-save.png` });
} finally {
  await browser.close();
}
