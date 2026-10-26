// affected.mjs: the list of unfinished runs a change would refuse, in real Chrome (flowedit/affected_browser_test.go
// starts the server over a store of unfinished runs, typed). The panel is there because the server has a store; the
// button is reached and pressed with the keyboard alone; the answer is a table with a caption and header cells, its
// words in text, focus on the headline; an edit of the text says the list is out of date; axe-core runs over the
// panel's states in the light, dark and high-contrast themes. It prints one JSON line.
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
  await page.setBypassCSP(true);
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
  const panel = () => page.evaluate(() => ({
    hidden: document.getElementById('affected-panel').hidden,
    heading: document.getElementById('h-affected').textContent,
    runPanelHidden: document.getElementById('run-panel').hidden,
    button: document.getElementById('aff-check').textContent,
    againHidden: document.getElementById('aff-again').hidden,
    word: document.getElementById('aff-word')?.textContent ?? '',
    line: document.querySelector('#aff-result .vline')?.textContent ?? '',
    caption: document.querySelector('#aff-result caption')?.textContent ?? '',
    colHeaders: [...document.querySelectorAll('#aff-result thead th')].map((t) => t.textContent + ':' + t.getAttribute('scope')),
    rows: [...document.querySelectorAll('#aff-result tbody tr')].map((r) => ({ result: r.querySelector('th')?.textContent ?? '', scope: r.querySelector('th')?.getAttribute('scope') ?? '', cells: [...r.querySelectorAll('td')].map((c) => c.textContent) })),
    notes: [...document.querySelectorAll('#aff-result ul li')].map((l) => l.textContent),
    stale: document.getElementById('aff-stale').textContent,
    focus: document.activeElement?.id ?? '',
    live: document.getElementById('live').textContent,
  }));

  await until(() => !document.getElementById('affected-panel').hidden, null, 'the panel');
  await rest('as opened');
  const before = await panel();
  await check('unfinished runs, before the check');

  // The keyboard alone: Tab to the button, Enter.
  await page.evaluate(() => document.getElementById('source').blur());
  let reached = false;
  for (let i = 0; i < 400 && !reached; i++) {
    await page.keyboard.press('Tab');
    reached = await page.evaluate(() => document.activeElement?.id === 'aff-check');
  }
  if (!reached) throw new Error('Tab never reached the button');
  await page.keyboard.press('Enter');
  await until(() => document.getElementById('aff-word'), null, 'the answer');
  await rest('answered');
  const listed = await panel();
  await check('unfinished runs, listed');

  // An edit makes the list out of date, and says so; the list stays.
  await page.$eval('#source', (e) => { e.value = e.value + ' '; e.dispatchEvent(new Event('input', { bubbles: true })); });
  await rest('edited');
  const stale = await panel();
  out = { ok: true, before, reached, listed, stale, results, console: console_, failed, offOrigin };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), results, console: console_, failed, offOrigin };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
