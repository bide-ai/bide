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
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
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
  // Open every disclosure: the controls inside are part of what the themes must reach.
  await page.evaluate(() => document.querySelectorAll('details').forEach((d) => { d.open = true; }));

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
    const control = (sel) => { const e = document.querySelector(sel); const bc = rgb(cs(e, 'borderTopColor')); return { sel, width: parseFloat(cs(e, 'borderTopWidth')), color: cs(e, 'borderTopColor'), strong: probe(token('--line-strong')), ratio: ratio(bc, under(e.parentElement)) }; };
    const sel = document.querySelector('#canvas .node.sel .box');
    const b = document.getElementById('theme');
    const t = window.bideTheme;
    return {
      bgToken: token('--bg'), attr: root.getAttribute('data-theme'), current: t.current(), names: t.names(), forced: t.forced(),
      switchHidden: b.hidden, switchDisplay: cs(b, 'display'), switchLabel: b.getAttribute('aria-label'),
      primary: { ratio: ratio(primary.text, primary.fill), text: 'rgb(' + primary.text.slice(0, 3).join(', ') + ')', onAccent: probe(token('--on-accent')), fill: 'rgb(' + primary.fill.slice(0, 3).join(', ') + ')', accent: probe(token('--accent')) },
      controls: ['#undo', '#source', '#outline button'].map(control),
      selStroke: sel ? cs(sel, 'stroke') : '', focus: probe(token('--focus')),
      selWidth: sel ? parseFloat(cs(sel, 'strokeWidth')) : 0,
      otherWidth: Math.max(...[...document.querySelectorAll('#canvas .node:not(.sel) .box')].map((e) => parseFloat(cs(e, 'strokeWidth')))),
      scrim: probe(token('--scrim')), lineStrong: probe(token('--line-strong')),
      colorScheme: Object.fromEntries(['html', '#source', '#pal-q', '#palette', 'input[type=checkbox]'].map((sel) => { const e = document.querySelector(sel); return [sel, e ? cs(e, 'colorScheme') : null]; })),
    };
  });

  // Every visible control, with every disclosure open and a node selected: its edge is the border_strong
  // token (a selected or primary control marks its state with the accent instead) and a text control
  // sits on the card token. A control the theme does not reach is listed.
  const audit = () => page.evaluate(() => {
    const probe = (value) => { const e = document.createElement('i'); e.style.color = value; document.body.append(e); const c = getComputedStyle(e).color; e.remove(); return c; };
    const root = document.documentElement;
    const token = (n) => getComputedStyle(root).getPropertyValue(n).trim();
    const strong = probe(token('--line-strong')), card = probe(token('--card'));
    const bad = [];
    let seen = 0;
    for (const e of document.querySelectorAll('input, select, textarea, button, summary')) {
      if (e.getClientRects().length === 0 || e.type === 'checkbox' || e.type === 'radio' || e.type === 'hidden' || e.tagName === 'SUMMARY') continue;
      if (e.classList.contains('primary') || e.getAttribute('aria-current') === 'true') continue;
      const s = getComputedStyle(e);
      seen++;
      const id = e.id || e.className || e.tagName;
      if (parseFloat(s.borderTopWidth) > 0 && s.borderTopColor !== strong) bad.push(id + ' edge ' + s.borderTopColor + ' want ' + strong);
      if (e.tagName !== 'BUTTON' && s.backgroundColor !== card) bad.push(id + ' fill ' + s.backgroundColor + ' want ' + card);
    }
    return { seen, bad };
  });

  // The dialog dims the page and keeps its own edge: the colour of one pixel far from the dialog,
  // before and after it opens, and the dialog's border.
  const dim = async () => {
    const px = async (x = 2, y = 2) => {
      const b64 = await page.screenshot({ clip: { x, y, width: 1, height: 1 }, encoding: 'base64' });
      return page.evaluate(async (b) => {
        const img = await new Promise((res, rej) => { const i = new Image(); i.onload = () => res(i); i.onerror = rej; i.src = 'data:image/png;base64,' + b; });
        const c = document.createElement('canvas'); c.width = img.width; c.height = img.height;
        const g = c.getContext('2d'); g.drawImage(img, 0, 0);
        return [...g.getImageData(0, 0, 1, 1).data].slice(0, 3);
      }, b64);
    };
    // The corner of the box, outside its rounded edge: it is page, and must be dimmed with the rest.
    await page.evaluate(() => document.getElementById('dialog').showModal());
    const box = await page.evaluate(() => { const f = document.getElementById('dialog-form'), r = f.getBoundingClientRect(); return { x: Math.floor(r.left) + 1, y: Math.floor(r.top) + 1, radius: parseFloat(getComputedStyle(f).borderTopLeftRadius) }; });
    await page.evaluate(() => document.getElementById('dialog').close());
    const before = await px();
    const cornerBefore = await px(box.x, box.y);
    await page.evaluate(() => document.getElementById('dialog').showModal());
    const after = await px();
    const cornerAfter = await px(box.x, box.y);
    const edge = await page.evaluate(() => {
      const d = document.getElementById('dialog-form'), s = getComputedStyle(d);
      // The elevation is the theme's shadow token, on the box that is drawn (not on the dim carrier).
      const t = document.createElement('i'); t.style.boxShadow = 'var(--shadow)'; document.body.append(t);
      const want = getComputedStyle(t).boxShadow; t.remove();
      return { width: parseFloat(s.borderTopWidth), color: s.borderTopColor, shadow: s.boxShadow, wantShadow: want, overflowY: s.overflowY };
    });
    // Content taller than the viewport scrolls inside the box, which stays on screen.
    const tall = await page.evaluate(() => {
      const body = document.getElementById('dialog-body');
      const filler = document.createElement('div');
      for (let i = 0; i < 120; i++) { const p = document.createElement('p'); p.textContent = 'line ' + i; filler.append(p); }
      body.append(filler);
      const f = document.getElementById('dialog-form');
      const r = f.getBoundingClientRect();
      const scrollable = f.scrollHeight > f.clientHeight;
      f.scrollTop = 200;
      const moved = f.scrollTop > 0;
      const out = { onScreen: r.top >= 0 && r.bottom <= window.innerHeight, scrollable, moved };
      filler.remove();
      return out;
    });
    // Escape closes it, as before.
    await page.keyboard.press('Escape');
    const closed = await page.evaluate(() => !document.getElementById('dialog').open);
    edge.tall = tall; edge.escapeCloses = closed;
    return { before, after, edge, corner: { before: cornerBefore, after: cornerAfter, radius: box.radius } };
  };

  const mode = async (key, features, theme) => {
    await page.emulateMediaFeatures(features);
    await page.evaluate((t) => { if (t) document.documentElement.setAttribute('data-theme', t); else document.documentElement.removeAttribute('data-theme'); }, theme);
    // A media change reaches the page's listeners with the next frame: wait for two.
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
    const v = await measure();
    v.audit = await audit();
    v.dim = await dim();
    return [key, v];
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
      ...(env.CUSTOM ? [[env.CUSTOM, [], env.CUSTOM]] : []),
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
