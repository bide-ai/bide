// smoke.mjs: drives bide edit's page in real Chrome (flowedit/browser_test.go starts the server and
// runs this). It opens PAGE_URL, reads what the page drew, selects a node with the pointer and then
// with the keyboard alone, and prints one JSON line with what it saw and what the browser did: the
// console messages (a Content-Security-Policy violation is one), failed requests, and every request
// that left the page's origin.
import puppeteer from 'puppeteer-core';

const env = process.env;
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];

const browser = await puppeteer.launch({
  executablePath: env.CHROME_BIN,
  userDataDir: env.BIDE_CHROME_PROFILE,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  timeout: 60000,
});
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) offOrigin.push(r.url()); });
  await page.setViewport({ width: 1400, height: 900 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });

  const text = (sel) => page.$eval(sel, (e) => e.textContent);
  const verdict = await text('#vword');
  const outline = await page.$$eval('#outline button', (bs) => bs.map((b) => b.getAttribute('data-node')));
  const graphNodes = await page.$$eval('#canvas g.node', (gs) => gs.map((g) => g.getAttribute('data-id')));
  const svgNamespace = await page.$eval('#canvas svg', (s) => s.namespaceURI);
  const source = await page.$eval('#source', (e) => e.value);
  const assertions = await page.$$eval('#assertions .assert', (a) => a.map((e) => e.textContent.trim()));
  const inspectorBefore = await text('#inspector');
  const banner = await page.$eval('#weakened', (e) => ({ text: e.textContent, role: e.querySelector('section')?.getAttribute('role') ?? '', controls: e.querySelectorAll('button, a, [tabindex]').length }));
  const baselineLine = await text('#baseline');

  // The pointer: click a node in the graph.
  await page.click('#canvas g.node[data-id="charge"]');
  const byPointer = await text('#inspector h3');
  // The selection is not told by color alone: the selected item has a heavier left edge, a bold name and a marker.
  const cue = await page.evaluate(() => {
    const sel = document.querySelector('#outline button[aria-current="true"]');
    const other = document.querySelector('#outline button[aria-current="false"]');
    const w = (b) => ({ edge: parseFloat(getComputedStyle(b).borderLeftWidth), weight: Number(getComputedStyle(b.querySelector('.nm')).fontWeight), marker: getComputedStyle(b.querySelector('.nm'), '::before').content });
    return { sel: w(sel), other: w(other) };
  });
  const current = await page.$$eval('#outline button[aria-current="true"]', (bs) => bs.map((b) => b.getAttribute('data-node')));

  // The keyboard alone: from the top of the page, Tab to an outline button and press Enter.
  await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
  let reached = '';
  for (let i = 0; i < 60 && !reached; i++) {
    await page.keyboard.press('Tab');
    reached = await page.evaluate(() => (document.activeElement?.closest('#outline') ? document.activeElement.getAttribute('data-node') : ''));
  }
  await page.keyboard.press('Enter');
  const byKeyboard = await text('#inspector h3');
  const live = await text('#live');

  // The graph node by keyboard: focus it and press Enter.
  await page.$eval('#canvas g.node[data-id="validate"]', (g) => g.focus());
  await page.keyboard.press('Enter');
  const byKeyboardGraph = await text('#inspector h3');

  out = { ok: true, cue, banner, baselineLine, verdict, outline, graphNodes, svgNamespace, source, assertions, inspectorBefore, byPointer, current, reached, byKeyboard, byKeyboardGraph, live, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
