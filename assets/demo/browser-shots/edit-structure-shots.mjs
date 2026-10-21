// edit-structure-shots.mjs URL OUTDIR: the editor guide's picture of a structure edit in progress,
// docs/guides/img/flow-edit-structure.png (light, 1280 x 1150), from the real page. A node is added with the
// Add a node form and wired in after receipt with the Connections form (so the page is green), and then the port at the right edge of the charge node is
// pressed and dragged toward it: the picture is taken with the button still down, the drag line drawn from the
// port to the pointer, the charge node selected in the outline and the inspector showing its Connections form.
// The drag is then cancelled with Escape, so nothing is written.
import puppeteer from 'puppeteer-core';

const [url, outDir] = process.argv.slice(2);
if (!url || !outDir) {
  console.error('usage: edit-structure-shots.mjs URL OUTDIR');
  process.exit(2);
}
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run', '--hide-scrollbars', '--force-device-scale-factor=1', '--font-render-hinting=none'] });
try {
  const page = await browser.newPage();
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }, { name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.setViewport({ width: 1280, height: 1150, deviceScaleFactor: 1 });
  await page.goto(url, { waitUntil: 'networkidle0', timeout: 60000 });
  await page.evaluate(() => document.fonts.ready);
  const until = (fn, what) => page.waitForFunction(fn, { timeout: 15000 }).catch(() => { throw new Error('timed out waiting for ' + what); });
  await page.$eval('#outline button[data-node="charge"]', (b) => b.click());
  await until(() => document.getElementById('c-edge-add'), 'the Connections form');
  await page.type('#an-name', 'notify');
  await page.type('#an-block', 'notify');
  await page.click('#an-add');
  await until(() => document.querySelector('#canvas g.node[data-id="notify"]'), 'the new node in the drawing');
  // Wire it in after receipt with the Connections form, so the flow loads and the page is green.
  await page.$eval('#outline button[data-node="receipt"]', (b) => b.click());
  await until(() => document.getElementById('c-edge-add'), 'the Connections form of receipt');
  await page.select('#c-edge-to', 'notify');
  await page.click('#c-edge-add');
  await until(() => document.querySelector('#verdict .verdict') && !document.querySelector('#verdict .v-bad'), 'a clean verdict');
  await page.$eval('#outline button[data-node="charge"]', (b) => b.click());
  await until(() => document.getElementById('c-edge-add'), 'the Connections form');
  await page.evaluate(() => { document.activeElement && document.activeElement.blur(); window.scrollTo(0, 0); });
  const centre = async (sel) => page.$eval(sel, (e) => { const r = e.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; });
  const port = await centre('#canvas g.node[data-id="charge"] .port-hit-box');
  const target = await centre('#canvas g.node[data-id="notify"] .box');
  await page.mouse.move(port.x, port.y);
  await page.mouse.down();
  for (let i = 1; i <= 10; i++) await page.mouse.move(port.x + ((target.x - port.x) * i) / 10, port.y + ((target.y - port.y) * i) / 10);
  await until(() => document.querySelector('#canvas line.drag'), 'the drag line');
  await new Promise((r) => setTimeout(r, 300));
  await page.screenshot({ path: `${outDir}/flow-edit-structure.png` });
  await page.keyboard.press('Escape');
  await page.mouse.up();
} finally {
  await browser.close();
}
