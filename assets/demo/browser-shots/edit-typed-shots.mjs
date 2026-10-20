// edit-typed-shots.mjs URL OUTDIR: the editor guide's pictures of typed mode (bide edit -blocks), from the real
// page, light, 1280 wide at 1x. The editor is open on a refund flow committed to git, with the blocks the
// program registers:
//   flow-edit-typed.png            the typed tag, the verdict and the digest disclosure (open) beside the flow
//   flow-edit-typed-blocks.png     the Blocks region: every registered block with its types, safety and gate
//   flow-edit-typed-uncertified.png  the source edited so that the gate is weakened and a step's types do not fit:
//                                  the weakened banner and the NOT CERTIFIED banner side by side
//   flow-edit-typed-save.png       the save dialog over it: one confirmation box for each statement
import puppeteer from 'puppeteer-core';

const [url, outDir] = process.argv.slice(2);
if (!url || !outDir) {
  console.error('usage: edit-typed-shots.mjs URL OUTDIR');
  process.exit(2);
}
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run', '--hide-scrollbars', '--force-device-scale-factor=1', '--font-render-hinting=none'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: 1280, height: 900, deviceScaleFactor: 1 });
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  await page.evaluate(() => document.fonts.ready);
  const until = (fn, what) => page.waitForFunction(fn, { timeout: 15000 }).catch(() => { throw new Error('timed out waiting for ' + what); });
  const settle = async () => {
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    await page.evaluate(() => window.scrollTo(0, 0));
    await new Promise((r) => setTimeout(r, 300));
  };

  // The page is typed: the tag says so, and the digest is there while the config is clean.
  await until(() => !document.getElementById('typed-tag').hidden && document.getElementById('digest'), 'the typed tag and the digest');
  await page.$eval('#outline button[data-node="charge"]', (b) => b.click());
  await until(() => document.getElementById('g-apply'), 'the charge node\'s forms');
  await page.$eval('#digest', (d) => { d.open = true; });
  await settle();
  await page.screenshot({ path: `${outDir}/flow-edit-typed.png` });

  // The Blocks region, with the charge node selected: the palette rows and their words.
  await until(() => document.querySelectorAll('#palette li').length >= 6, 'the palette rows');
  // The list scrolls inside a box of 20rem; the picture shows every row, so the box is let grow for it.
  await page.$eval('#palette', (e) => { e.style.maxHeight = 'none'; });
  const box = await page.$eval('#blocks', (e) => { const r = e.getBoundingClientRect(); return { y: r.top + window.scrollY, h: r.height }; });
  await page.screenshot({ path: `${outDir}/flow-edit-typed-blocks.png`, captureBeyondViewport: true, clip: { x: 0, y: Math.max(0, box.y - 12), width: 1280, height: box.h + 24 } });
  await page.$eval('#palette', (e) => { e.style.maxHeight = ''; });

  // The source edited: the separation-of-duties line removed, and the receipt step replaced by a block that
  // takes a string, so the edge into it does not fit. The text can be saved, and cannot be certified.
  const src = await page.$eval('#source', (e) => e.value);
  let edited = src.replace(/^\s*exclude_principal: true\n/m, '').replace(/, exclude_principal: true/, '');
  edited = edited.replace(/block: receipt/, 'block: notify');
  if (edited === src || !edited.includes('block: notify')) throw new Error('the flow has nothing to edit');
  await page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, edited);
  await until(() => document.querySelector('#weakened section[role="alert"]') && document.querySelectorAll('#weakened section[role="alert"]').length >= 2, 'the weakened and NOT CERTIFIED banners');
  await settle();
  await page.screenshot({ path: `${outDir}/flow-edit-typed-uncertified.png` });

  await page.click('#save');
  await until(() => document.getElementById('dialog').open && document.querySelectorAll('#dialog input[type="checkbox"]').length === 2, 'the save dialog with two confirmation boxes');
  await new Promise((r) => setTimeout(r, 300));
  await page.screenshot({ path: `${outDir}/flow-edit-typed-save.png` });
} finally {
  await browser.close();
}
