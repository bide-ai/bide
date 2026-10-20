// themes.mjs: bide edit's page under each theme and system mode, in real Chrome (flowedit/themes_browser_test.go
// starts the server and runs this). It measures what the browser painted, not what the stylesheet says:
// the primary button's text against its fill (on_accent), the edge of a control against what it sits
// on (border_strong), the selected node's ring against the focus token, the dialog's backdrop against
// the scrim token, and the theme switch: that it steps through the stylesheet's themes, names the next,
// and is hidden when the page has one theme (a host locked it). MODE=default runs the switch and the
// modes; MODE=locked runs a page the host locked to one theme, under the same system modes. It prints
// one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const console_ = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') console_.push(`${m.type()}: ${m.text()}`); });
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  await page.setViewport({ width: 1400, height: 1000 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  const atRest = () => page.waitForFunction(() => document.documentElement.dataset.busy === '0', { timeout: 10000 });
  await atRest();
  await page.click('#outline button[data-node="charge"]');
  await atRest();

  // What the page painted in the current state. Colours are resolved by the browser; the contrast
  // ratios are WCAG 2.x.
  const measure = () => page.evaluate(() => {
    const rgb = (s) => { const m = /rgba?\(([^)]+)\)/.exec(s); if (!m) return null; const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number); return p.length === 3 ? [...p, 1] : p; };
    const lum = ([r, g, b]) => { const f = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
    const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
    const cs = (el, p, pseudo) => getComputedStyle(el, pseudo)[p];
    const root = document.documentElement;
    const token = (n) => getComputedStyle(root).getPropertyValue(n).trim();
    // A probe element resolves a colour (a token, a system colour) to what the browser paints.
    const probe = (value) => { const e = document.createElement('i'); e.style.color = value; document.body.append(e); const c = getComputedStyle(e).color; e.remove(); return c; };
    // What an element sits on: the first opaque background up the tree.
    const under = (el) => { for (let e = el; e; e = e.parentElement) { const c = rgb(cs(e, 'backgroundColor')); if (c && c[3] === 1) return c; } return rgb(cs(document.body, 'backgroundColor')) || [255, 255, 255, 1]; };
    const sample = document.createElement('button');
    sample.className = 'btn primary'; sample.textContent = 'x';
    document.querySelector('.toolbar').append(sample);
    const primary = { text: rgb(cs(sample, 'color')), fill: rgb(cs(sample, 'backgroundColor')) };
    sample.remove();
    const control = (sel) => { const e = document.querySelector(sel); const bc = rgb(cs(e, 'borderTopColor')); return { sel, width: parseFloat(cs(e, 'borderTopWidth')), color: cs(e, 'borderTopColor'), ratio: ratio(bc, under(e.parentElement)) }; };
    const dlg = document.getElementById('dialog');
    dlg.showModal();
    const backdrop = cs(dlg, 'backgroundColor', '::backdrop');
    dlg.close();
    const sel = document.querySelector('#canvas .node.sel .box');
    const b = document.getElementById('theme');
    const t = window.bideTheme;
    return {
      bgToken: token('--bg'), attr: root.getAttribute('data-theme'), current: t.current(), names: t.names(), forced: t.forced(),
      switchHidden: b.hidden, switchDisplay: cs(b, 'display'), switchLabel: b.getAttribute('aria-label'),
      primary: { ratio: ratio(primary.text, primary.fill), text: 'rgb(' + primary.text.slice(0, 3).join(', ') + ')', onAccent: probe(token('--on-accent')), fill: 'rgb(' + primary.fill.slice(0, 3).join(', ') + ')', accent: probe(token('--accent')) },
      controls: ['.btn', '#source', '#outline button'].map(control),
      selStroke: sel ? cs(sel, 'stroke') : '', focus: probe(token('--focus')),
      backdrop, scrim: probe(token('--scrim')),
    };
  });

  const mode = async (key, features, theme) => {
    await page.emulateMediaFeatures(features);
    await page.evaluate((t) => { if (t) document.documentElement.setAttribute('data-theme', t); else document.documentElement.removeAttribute('data-theme'); }, theme);
    // A media change reaches the page's listeners with the next frame: wait for two.
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
    return [key, await measure()];
  };

  const results = {};
  if (env.MODE === 'locked') {
    for (const [k, f, t] of [
      ['locked', [], null],
      ['locked+system-dark', [{ name: 'prefers-color-scheme', value: 'dark' }], null],
      ['locked+set-in-page', [], 'light'],
      ['locked+prefers-contrast', [{ name: 'prefers-contrast', value: 'more' }], null],
      ['locked+prefers-contrast+set-in-page', [{ name: 'prefers-contrast', value: 'more' }], 'dark'],
      ['locked+forced-colors', [{ name: 'forced-colors', value: 'active' }], null],
    ]) { const [key, v] = await mode(k, f, t); results[key] = v; }
    // The switch cannot be used: a click and the key do nothing.
    await page.emulateMediaFeatures([]);
    await page.evaluate(() => document.documentElement.removeAttribute('data-theme'));
    await page.evaluate(() => document.getElementById('theme').click());
    results.afterClick = await page.evaluate(() => ({ attr: document.documentElement.getAttribute('data-theme'), token: getComputedStyle(document.documentElement).getPropertyValue('--bg').trim() }));
  } else {
    for (const [k, f, t] of [
      ['light', [], 'light'],
      ['dark', [], 'dark'],
      ['high-contrast', [], 'high-contrast'],
      ['prefers-contrast', [{ name: 'prefers-contrast', value: 'more' }], null],
      ['forced-colors', [{ name: 'forced-colors', value: 'active' }], 'dark'],
    ]) { const [key, v] = await mode(k, f, t); results[key] = v; }
    // The switch: from a clean page, it steps through the stylesheet's list, naming the next theme,
    // and returns to the start.
    await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }]);
    await page.evaluate(() => { try { localStorage.clear(); } catch (e) { /* none */ } });
    await page.reload({ waitUntil: 'networkidle0' });
    const steps = [];
    const names = await page.evaluate(() => window.bideTheme.names());
    for (let i = 0; i <= names.length; i++) {
      steps.push(await page.evaluate(() => ({ current: window.bideTheme.current(), label: document.getElementById('theme').getAttribute('aria-label'), token: getComputedStyle(document.documentElement).getPropertyValue('--bg').trim(), status: document.getElementById('theme-status').textContent })));
      await page.click('#theme');
    }
    results.cycle = { names, steps };
    // A choice (the last of the list, which the page's script cannot know before the stylesheet has
    // loaded) is remembered across a reload.
    await page.evaluate(() => { try { localStorage.clear(); } catch (e) { /* none */ } });
    await page.reload({ waitUntil: 'networkidle0' });
    for (let i = 0; i < names.length - 1; i++) await page.click('#theme');
    const chosen = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
    await page.reload({ waitUntil: 'networkidle0' });
    results.remembered = { chosen, after: await page.evaluate(() => document.documentElement.getAttribute('data-theme')) };
  }
  out = { ok: true, results, console: console_ };
} catch (e) {
  out = { ok: false, error: String((e && e.message) || e), console: console_ };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
