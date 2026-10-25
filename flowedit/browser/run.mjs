// run.mjs: bide edit -run in real Chrome (flowedit/run_browser_test.go starts the server, typed, on a
// two-node flow whose run journaled the same flow). It reads the panel as opened (same digest), changes a
// block in the source and reads it again (differs), presses Reload run with the keyboard alone, and runs
// axe-core over the panel in each state in the light, dark and high-contrast themes. It prints one JSON
// line: what the panel said, the violations per state, the live region and what the browser did.
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
  await page.setBypassCSP(true); // for axe only, as in a11y.mjs
  await page.setViewport({ width: 1400, height: 1400 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.evaluate(axeSource);
  const rest = (what) => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 }).catch(() => { throw new Error('timed out waiting for the page to be at rest: ' + what); });
  const until = (fn, arg, what) => page.waitForFunction(fn, { timeout: 10000 }, arg).catch(() => { throw new Error('timed out waiting for ' + what); });
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
  const panel = () => page.evaluate(() => ({
    hidden: document.getElementById('run-panel').hidden,
    heading: document.getElementById('h-run').textContent,
    labelled: document.getElementById('run-panel').getAttribute('aria-labelledby'),
    facts: document.getElementById('run-facts').textContent,
    word: document.getElementById('run-word')?.textContent ?? '',
    claim: document.querySelector('#run-claim .vline')?.textContent ?? '',
    icon: !!document.querySelector('#run-claim .vicon svg'),
    lists: [...document.querySelectorAll('#run-detail ul')].map((u) => ({ label: document.getElementById(u.getAttribute('aria-labelledby'))?.textContent ?? '', items: [...u.children].map((l) => l.textContent) })),
    read: document.getElementById('run-read').textContent,
  }));

  await until(() => !document.getElementById('run-panel').hidden && document.getElementById('run-word'), null, 'the run panel');
  const same = await panel();
  const line = await page.evaluate(() => {
    const l = document.getElementById('run-line');
    const a = document.getElementById('run-line-link');
    const canvas = document.getElementById('canvas');
    const panel = document.getElementById('run-panel');
    return {
      hidden: l.hidden, word: document.getElementById('run-line-word').textContent, text: document.getElementById('run-line-text').textContent,
      link: a.textContent, aboveFlow: !!(l.compareDocumentPosition(canvas) & Node.DOCUMENT_POSITION_FOLLOWING),
      panelBelowFlow: !!(canvas.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING),
    };
  });
  // The link is reached by keyboard and moves focus to the panel's heading.
  await page.evaluate(() => document.getElementById('source').blur());
  let linkReached = false;
  for (let i = 0; i < 400 && !linkReached; i++) {
    await page.keyboard.press('Tab');
    linkReached = await page.evaluate(() => document.activeElement?.id === 'run-line-link');
  }
  if (!linkReached) throw new Error('Tab never reached the details link');
  await page.keyboard.press('Enter');
  const focusAfterLink = await page.evaluate(() => document.activeElement?.id ?? '');
  await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
  await check('run panel, same digest');

  // Change the block of node b in the source: the digest no longer matches.
  await page.$eval('#source', (e) => { e.value = e.value.replace('"block":"dbl"', '"block":"inc"'); e.dispatchEvent(new Event('input', { bubbles: true })); });
  await until(() => document.getElementById('run-word')?.textContent === 'DIFFERS FROM THE RUN', null, 'the panel to read DIFFERS');
  await rest('differs');
  const differs = await panel();
  const liveAfterEdit = await page.$eval('#live', (e) => e.textContent);
  await check('run panel, differs');

  // The keyboard alone: Tab to Reload run and press Enter.
  await page.evaluate(() => document.getElementById('source').blur());
  let reached = false;
  for (let i = 0; i < 400 && !reached; i++) {
    await page.keyboard.press('Tab');
    reached = await page.evaluate(() => document.activeElement?.id === 'run-reload');
  }
  if (!reached) throw new Error('Tab never reached Reload run');
  const reloadLabel = await page.$eval('#run-reload', (b) => b.textContent);
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('live').textContent.includes('Run read again'), null, 'the reload to be announced');
  await rest('reloaded');
  const liveAfterReload = await page.$eval('#live', (e) => e.textContent);
  const afterReload = await panel();
  out = { ok: true, line, linkReached, focusAfterLink, same, differs, afterReload, liveAfterEdit, liveAfterReload, reloadLabel, results, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), results, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
