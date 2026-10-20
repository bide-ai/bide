// a11y.mjs: runs axe-core (pinned by the lockfile) over bide edit's page in real Chrome, in the light
// and the dark theme and in the states that matter: the page as opened, a node selected with its forms,
// a weakening banner, an assertion form with a live violation, and the save dialog with its
// confirmation. An automated check finds a fraction of problems (see docs/guides/flow-editor.md,
// "Accessibility"); the keyboard-only run and the manual screen-reader pass are the gate, and this
// catches the regressions a script can. It prints one JSON line: the violations per state.
import fs from 'node:fs';
import { createRequire } from 'node:module';
import puppeteer from 'puppeteer-core';

const env = process.env;
const require = createRequire(import.meta.url);
const axeSource = fs.readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8');
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
let page;
const results = {};
try {
  page = await browser.newPage();
  // The page's policy forbids an inline script, which is how axe is handed to it: bypass it for the
  // checker only. Nothing else in the run depends on this.
  await page.setBypassCSP(true);
  await page.setViewport({ width: 1400, height: 1400 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.evaluate(axeSource);
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
  const check = async (state) => {
    // The page says when it is at rest (data-busy on <html> counts its requests in flight and the
    // pauses it waits out before it asks the server), so axe never reads a half-drawn state and no
    // guess of how long a redraw takes is involved.
    await page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 }).catch(() => { throw new Error('timed out waiting for the page to be at rest in: ' + state); });
    for (const theme of ['light', 'dark', 'high-contrast']) {
      await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
      const r = await page.evaluate(() => window.axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'] } }));
      results[`${state} (${theme})`] = {
        violations: r.violations.map((v) => ({ id: v.id, impact: v.impact, help: v.help, nodes: v.nodes.map((n) => n.target.join(' ') + ' :: ' + String(n.html).slice(0, 200) + ' :: ' + String(n.failureSummary).replace(/\s+/g, ' ').slice(0, 300)).slice(0, 5) })),
        passes: r.passes.length,
      };
    }
  };
  await check('opened');

  await page.keyboard.press('Tab'); // anything focusable first: not a pointer path
  await page.$eval('#outline button[data-node="charge"]', (b) => b.click());
  await until(() => document.getElementById('g-apply'), null, 'the forms');
  await check('a gated node selected, with its forms');

  const src = await page.$eval('#source', (e) => e.value);
  await page.$eval('#source', (e, v) => { e.value = v; e.dispatchEvent(new Event('input', { bubbles: true })); }, src.replace('need: 2', 'need: 1'));
  await until(() => document.querySelector('#weakened section[role="alert"]'), null, 'the banner');
  await check('a weakened banner and a violated assertion');

  await page.$eval('#a-template', (e) => { e.value = 'no_side_effect'; e.dispatchEvent(new Event('change', { bubbles: true })); });
  await until(() => document.getElementById('a-node'), null, 'the assertion fields');
  await page.$eval('#a-node', (e) => { e.value = 'validate'; e.dispatchEvent(new Event('change', { bubbles: true })); });
  await until(() => document.getElementById('a-live')?.textContent.includes('violated'), null, 'the live verdict');
  await check('an assertion form with a live violation');

  await page.$eval('#save', (b) => b.click());
  await until(() => document.getElementById('dialog').open, null, 'the save dialog');
  await check('the save dialog with its confirmation');
  out = { ok: true, results };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), results };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
