// run-path.mjs: the run's path on the canvas, in real Chrome (flowedit/run_browser_test.go starts the server
// on a loop flow and a run of it). It reads the marks the page drew (a ring and words per node, a dashed line
// per connection taken, the words in the outline and in the node's accessible name), checks that each state
// is told by words and by the ring's dash pattern and not by color alone, including under forced colors,
// edits the file so one node is another block (its marks go and the panel lists it), and runs axe-core over
// each state in the light, dark and high-contrast themes. It prints one JSON line.
import fs from 'node:fs';
import { createRequire } from 'node:module';
import puppeteer from 'puppeteer-core';

const env = process.env;
const require = createRequire(import.meta.url);
const axeSource = fs.readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8');
const origin = new URL(env.PAGE_URL).origin;
const console_ = [];
const offOrigin = [];
const failed = [];
const results = {};
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) offOrigin.push(r.url()); });
  await page.setBypassCSP(true); // for axe only
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.evaluate(axeSource);
  const rest = (what) => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 15000 }).catch(() => { throw new Error('timed out waiting for rest: ' + what); });
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 15000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const check = async (state) => {
    await rest(state);
    for (const theme of ['light', 'dark', 'high-contrast']) {
      await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
      const r = await page.evaluate(() => window.axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'] } }));
      results[`${state} (${theme})`] = {
        violations: r.violations.map((v) => ({ id: v.id, impact: v.impact, help: v.help, nodes: v.nodes.map((n) => n.target.join(' ') + ' :: ' + String(n.html).slice(0, 200)).slice(0, 5) })),
        passes: r.passes.length,
      };
    }
    await page.evaluate(() => document.documentElement.removeAttribute('data-theme'));
  };
  const marks = () => page.evaluate(() => {
    const nodes = {};
    for (const g of document.querySelectorAll('#canvas g.node')) {
      const ring = g.querySelector('.rring');
      const mark = g.querySelector('.rmark');
      const title = [...g.children].find((c) => c.localName === 'title');
      nodes[g.getAttribute('data-id')] = {
        ring: ring ? ring.getAttribute('class') : '',
        dash: ring ? getComputedStyle(ring).strokeDasharray : '',
        width: ring ? parseFloat(getComputedStyle(ring).strokeWidth) : 0,
        stroke: ring ? getComputedStyle(ring).stroke : '',
        words: mark ? mark.textContent : '',
        markHidden: mark ? mark.getAttribute('aria-hidden') : '',
        title: title ? title.textContent : '',
      };
    }
    const taken = [...document.querySelectorAll('#canvas g.edge-g .rtaken')].map((p) => p.parentElement.getAttribute('data-from') + '>' + p.parentElement.getAttribute('data-to'));
    const outline = {};
    for (const b of document.querySelectorAll('#outline button[data-node]')) outline[b.getAttribute('data-node')] = b.querySelector('.rwords')?.textContent ?? '';
    const lists = [...document.querySelectorAll('#run-detail ul')].map((u) => ({ label: document.getElementById(u.getAttribute('aria-labelledby'))?.textContent ?? '', items: [...u.children].map((l) => l.textContent) }));
    return { nodes, taken, outline, lists, legend: document.getElementById('run-legend')?.textContent ?? '' };
  });

  await until(() => document.querySelector('#canvas .rring'), null, 'the marks');
  await rest('as opened');
  const opened = await marks();
  await check('run path, as opened');

  // Forced colors: the system's colors win, so the ring is still drawn (a stroke and a width) and the words stay.
  await page.emulateMediaFeatures([{ name: 'forced-colors', value: 'active' }]);
  const forced = await marks();
  await page.emulateMediaFeatures([{ name: 'forced-colors', value: 'none' }]);

  // The same text typed again draws the same marks (they are cleared and drawn again, not stacked).
  await page.$eval('#source', (e) => { e.value = e.value + ' '; e.dispatchEvent(new Event('input', { bubbles: true })); });
  await rest('typed again');
  await until(() => document.querySelector('#canvas .rring'), null, 'the marks again');
  const again = await marks();

  // One node becomes another block: its marks go, and the panel lists what the run did there.
  await page.$eval('#source', (e) => { e.value = e.value.replace('"name":"refine","block":"refine"', '"name":"refine","block":"check"'); e.dispatchEvent(new Event('input', { bubbles: true })); });
  await until(() => !document.querySelector('#canvas g.node[data-id="refine"] .rring'), null, 'the refine marks to go');
  await rest('edited');
  const edited = await marks();
  await check('run path, a node of another block');
  out = { ok: true, opened, forced, again, edited, results, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), results, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
