// colouronly.mjs: loads bide view's pages in real Chrome and reports, for each state the audit in
// docs/design/themes.md says must not differ by colour alone, what the browser computed: the dash
// pattern, width and cap of each edge, the stroke dash and width of each node box, the shape of each
// node mark, the glyph a label or a row gets, and the badge a new row gets. Each measured element
// also comes with a visibility record (display, visibility, opacity up the tree, stroke and text
// alpha, size, bounding box) and a pixel control: under the browser's achromatopsia emulation, the
// element's box is photographed with and without the element, and what changed is counted, so an
// element that is drawn in a shape but never painted fails. Computed styles, not CSS text: the
// cascade, specificity and whitespace are the browser's. The Go test
// (internal/runview/browser_test.go) judges what this prints.
//
// PAGES is a JSON object of page name to URL: config, configHeld, diff, run, runs. Prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const pages = JSON.parse(env.PAGES);
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
const out = { ok: false, themes: {}, console: [] };

// Helpers that run in the page.
const helpers = () => {
  const alpha = (c) => {
    if (!c || c === 'none' || c === 'transparent') return 0;
    const slash = c.match(/\/\s*([\d.]+%?)\s*\)/);
    if (slash) return slash[1].endsWith('%') ? parseFloat(slash[1]) / 100 : parseFloat(slash[1]);
    const m = c.match(/^rgba?\(([^)]*)\)/);
    if (m) { const p = m[1].split(',').map((x) => x.trim()); return p.length === 4 ? parseFloat(p[3]) : 1; }
    return 1;
  };
  const rgb = (c) => {
    const m = (c || '').match(/rgba?\(([^)]*)\)/);
    if (!m) return null;
    const p = m[1].split(/[,\s/]+/).filter(Boolean).map(parseFloat);
    return [p[0], p[1], p[2], p.length > 3 ? p[3] : 1];
  };
  // The nearest ancestor's background that is (mostly) opaque: what a shape or a glyph is drawn on.
  const bgOf = (el) => {
    for (let e = el; e && e.nodeType === 1; e = e.parentElement) {
      const c = rgb(getComputedStyle(e).backgroundColor);
      if (c && c[3] >= 0.5) return c.slice(0, 3);
    }
    return [255, 255, 255];
  };
  const chainOpacity = (el) => {
    let o = 1;
    for (let e = el; e && e.nodeType === 1; e = e.parentElement) o *= parseFloat(getComputedStyle(e).opacity);
    return o;
  };
  const chainShown = (el) => {
    for (let e = el; e && e.nodeType === 1; e = e.parentElement) {
      const s = getComputedStyle(e);
      if (s.display === 'none' || s.visibility === 'hidden' || s.visibility === 'collapse') return false;
    }
    return true;
  };
  // A drawn shape: stroke or fill, as the browser painted it.
  const shapeVis = (el) => {
    const s = getComputedStyle(el), r = el.getBoundingClientRect();
    return {
      shown: chainShown(el) && el.getClientRects().length > 0, opacity: chainOpacity(el),
      strokeAlpha: alpha(s.stroke) * parseFloat(s.strokeOpacity), strokeWidth: parseFloat(s.strokeWidth),
      fillAlpha: alpha(s.fill) * parseFloat(s.fillOpacity), w: r.width, h: r.height,
      stroke: (rgb(s.stroke) || [0, 0, 0]).slice(0, 3), fill: (rgb(s.fill) || [0, 0, 0]).slice(0, 3), bg: bgOf(el),
    };
  };
  // Text, or a pseudo-element's glyph, as the browser painted it.
  const textVis = (el, pseudo) => {
    const s = getComputedStyle(el, pseudo);
    return {
      shown: pseudo ? chainShown(el) && s.display !== 'none' && s.visibility !== 'hidden' && s.content !== 'none' && s.content !== 'normal' : chainShown(el) && el.getClientRects().length > 0,
      opacity: chainOpacity(el) * parseFloat(s.opacity), fontSize: parseFloat(s.fontSize), alpha: alpha(s.color), content: s.content,
      color: (rgb(s.color) || [0, 0, 0]).slice(0, 3), bg: bgOf(el),
    };
  };
  // The controls a page draws: what marks each one's edge, against what it sits on.
  const controls = () => [...document.querySelectorAll('input:not([type=hidden]), select, textarea, button, a.chip')]
    .filter((e) => chainShown(e) && e.getClientRects().length > 0 && e.getBoundingClientRect().width > 0)
    .map((e) => {
      const s = getComputedStyle(e), bc = rgb(s.borderTopColor) || [0, 0, 0, 0], fc = rgb(s.backgroundColor) || [0, 0, 0, 0];
      return { tag: e.tagName.toLowerCase(), id: e.id, cls: e.className, borderWidth: parseFloat(s.borderTopWidth), borderStyle: s.borderTopStyle, border: bc.slice(0, 3), borderAlpha: bc[3], fill: fc.slice(0, 3), fillAlpha: fc[3], parentBG: bgOf(e.parentElement), parentClass: e.parentElement ? e.parentElement.className : '' };
    });
  window.__h = { alpha, chainOpacity, chainShown, shapeVis, textVis, controls };
};

try {
  let mode = { theme: 'light' };
  const open = async (name) => {
    const page = await browser.newPage();
    page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') out.console.push(name + ': ' + m.text()); });
    page.on('pageerror', (e) => out.console.push(name + ': ' + e.message));
    await page.setViewport({ width: 1400, height: 1000 });
    await page.goto(pages[name], { waitUntil: 'networkidle0', timeout: 30000 });
    await page.evaluate(helpers);
    if (mode.features) await page.emulateMediaFeatures(mode.features);
    await page.evaluate((t) => { if (t) document.documentElement.setAttribute('data-theme', t); else document.documentElement.removeAttribute('data-theme'); }, mode.theme);
    await page.evaluate(() => { window.__bg = getComputedStyle(document.body).backgroundColor; window.__fg = getComputedStyle(document.body).color; window.__bgToken = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim(); });
    return page;
  };

  // paints: how many pixels of el's padded box change when el is hidden, under achromatopsia.
  const painter = async (page) => {
    const client = await page.createCDPSession();
    await client.send('Emulation.setEmulatedVisionDeficiency', { type: 'achromatopsia' });
    const shot = async (clip) => (await page.screenshot({ clip, encoding: 'base64' }));
    // pseudo: hide the element's ::before (visibility, so the layout stays) instead of the element,
    // through a rule added with CSSOM, which the page's policy allows.
    return async (handle, pseudo = false) => {
      await handle.evaluate((el) => el.scrollIntoView({ block: 'center', inline: 'center' }));
      if (pseudo) {
        await page.evaluate(() => {
          if (!window.__probeRule) { document.styleSheets[0].insertRule('[data-probe-hide]::before { visibility: hidden !important; }', 0); window.__probeRule = true; }
        });
      }
      const clip = await handle.evaluate((el) => {
        const r = el.getBoundingClientRect(), p = 6;
        return { x: Math.max(0, r.left + window.scrollX - p), y: Math.max(0, r.top + window.scrollY - p), width: Math.min(900, Math.max(8, r.width + 2 * p)), height: Math.min(500, Math.max(8, r.height + 2 * p)) };
      });
      const a = await shot(clip);
      if (pseudo) await handle.evaluate((el) => el.setAttribute('data-probe-hide', ''));
      else await handle.evaluate((el) => { el.dataset.was = el.style.visibility; el.style.visibility = 'hidden'; });
      const b = await shot(clip);
      if (pseudo) await handle.evaluate((el) => el.removeAttribute('data-probe-hide'));
      else await handle.evaluate((el) => { el.style.visibility = el.dataset.was; delete el.dataset.was; });
      return page.evaluate(async (x, y) => {
        const load = (b64) => new Promise((res, rej) => { const i = new Image(); i.onload = () => res(i); i.onerror = rej; i.src = 'data:image/png;base64,' + b64; });
        const [ia, ib] = await Promise.all([load(x), load(y)]);
        const px = (img) => { const c = document.createElement('canvas'); c.width = img.width; c.height = img.height; const g = c.getContext('2d'); g.drawImage(img, 0, 0); return g.getImageData(0, 0, c.width, c.height).data; };
        const da = px(ia), db = px(ib);
        let changed = 0;
        for (let i = 0; i < da.length; i += 4) {
          const la = 0.299 * da[i] + 0.587 * da[i + 1] + 0.114 * da[i + 2], lb = 0.299 * db[i] + 0.587 * db[i + 1] + 0.114 * db[i + 2];
          if (Math.abs(la - lb) >= 30) changed++;
        }
        return changed;
      }, a, b);
    };
  };

  // The edges, node boxes and node marks a graph page draws, as the browser computed them.
  const graph = async (page) => {
    const paints = await painter(page);
    const seen = new Set();
    const edges = [];
    for (const h of await page.$$('svg.graph path.edge')) {
      const d = await h.evaluate((e) => {
        const s = getComputedStyle(e);
        return { classes: [...e.classList].filter((c) => c !== 'back'), dash: s.strokeDasharray, width: parseFloat(s.strokeWidth), cap: s.strokeLinecap, len: e.getTotalLength(), vis: window.__h.shapeVis(e) };
      });
      const key = d.classes.join(' ');
      if (!seen.has(key)) { seen.add(key); d.paints = await paints(h); }
      edges.push(d);
    }
    const nodes = [];
    const seenNode = new Set();
    for (const h of await page.$$('svg.graph g.node')) {
      const d = await h.evaluate((n) => {
        const box = n.querySelector('.box'), s = getComputedStyle(box), mark = n.querySelector('.nmark');
        return {
          classes: [...n.classList], dash: s.strokeDasharray, width: parseFloat(s.strokeWidth), boxVis: window.__h.shapeVis(box),
          mark: mark ? { inside: (() => { const b = box.getBoundingClientRect(), m = mark.getBoundingClientRect(), cx = m.left + m.width / 2, cy = m.top + m.height / 2; return cx >= b.left - 2 && cx <= b.right + 2 && cy >= b.top - 2 && cy <= b.bottom + 2; })(), shape: mark.firstElementChild.tagName.toLowerCase(), text: mark.querySelector('text').textContent, shapeVis: window.__h.shapeVis(mark.firstElementChild), textVis: window.__h.textVis(mark.querySelector('text')) } : null,
        };
      });
      const key = d.classes.filter((c) => !c.startsWith('risk-')).join(' ');
      if (!seenNode.has(key)) {
        seenNode.add(key);
        d.boxPaints = await paints(await h.$('.box'));
        const m = await h.$('.nmark');
        if (m) {
          d.markPaints = await paints(m);
          // The "!" and the shape apart: a glyph in the shape's own colour paints nothing alone.
          d.markTextPaints = await paints(await m.$('text'));
        }
      }
      nodes.push(d);
    }
    return { edges, nodes };
  };

  // The stats a page draws, with the glyph each label gets and what it paints.
  const stats = async (page) => {
    const paints = await painter(page);
    const out = [];
    for (const lbl of await page.$$('.stat .lbl')) {
      const d = await lbl.evaluate((l) => ({ classes: [...l.parentElement.classList], label: l.textContent, glyph: window.__h.textVis(l, '::before') }));
      if (d.classes.some((c) => /^s-(good|bad|warn)$/.test(c))) d.paints = await paints(lbl, true);
      out.push(d);
    }
    return out;
  };

  // The three shipped themes, then the system asking for more contrast (no theme chosen in the page)
  // and the system forcing colors (a theme chosen in the page, which the system overrides).
  const modes = {
    'light': { theme: 'light' }, 'dark': { theme: 'dark' }, 'high-contrast': { theme: 'high-contrast' },
    'prefers-contrast': { theme: null, features: [{ name: 'prefers-contrast', value: 'more' }] },
    'forced-colors': { theme: 'dark', features: [{ name: 'forced-colors', value: 'active' }] },
  };
  for (const th of Object.keys(modes)) {
    mode = modes[th];
    const P = (out.themes[th] = {});
    for (const name of ['config', 'configHeld', 'diff', 'run', 'runWaiting', 'runHalted']) {
      const page = await open(name);
      P[name] = name === 'configHeld' ? {} : await graph(page);
      if (name === 'run') {
        // A node in the state class no fixture draws (attempted), computed by the same stylesheet.
        P[name].attemptedBox = await page.evaluate(() => {
          const n = document.querySelector('svg.graph g.node').cloneNode(true);
          n.setAttribute('class', 'node attempted');
          document.querySelector('svg.graph').appendChild(n);
          const s = getComputedStyle(n.querySelector('.box'));
          const r = { dash: s.strokeDasharray, width: parseFloat(s.strokeWidth) };
          n.remove();
          return r;
        });
      }
      P[name].body = await page.evaluate(() => ({ bg: window.__bg, fg: window.__fg, bgToken: window.__bgToken }));
      P[name].controls = await page.evaluate(() => { document.querySelectorAll('details').forEach((d) => { d.open = true; }); return window.__h.controls(); });
      P[name].stats = await stats(page);
      if (name === 'run') {
        // New timeline rows: the live script's marking, run on this page's own rows.
        await page.addStyleTag({ url: '/assets/live.css' });
        await page.addScriptTag({ url: '/assets/live.js' });
        const paints = await painter(page);
        P.run.fresh = await page.evaluate(() => {
          const rows = [...document.querySelectorAll('.timeline > li')];
          window.bideMarkFresh(rows, 1);
          return rows.map((r, i) => {
            const b = r.querySelector('.newbadge');
            return { index: i, fresh: r.classList.contains('fresh'), badge: b ? b.textContent : null, vis: b ? window.__h.textVis(b) : null };
          });
        });
        const badge = await page.$('.timeline > li:nth-child(2) .newbadge');
        P.run.badgePaints = badge ? await paints(badge) : 0;
      }
      await page.close();
    }

    // The run list: j puts the keyboard on a row; it is marked by a glyph and aria-current.
    {
      const page = await open('runs');
      await page.keyboard.press('j');
      const paintsRuns = await painter(page);
      const td = await page.$('table.runlist tbody tr.cur td:first-child');
      const markerPaints = td ? await paintsRuns(td, true) : 0;
      const controlsOfRuns = await page.evaluate(() => window.__h.controls());
      P.runs = await page.evaluate(() => {
        const r = document.querySelector('table.runlist tbody tr.cur');
        if (!r) return { cur: false };
        const td = r.querySelector('td:first-child');
        return { cur: true, ariaCurrent: r.getAttribute('aria-current'), glyph: window.__h.textVis(td, '::before'), others: document.querySelectorAll('table.runlist tbody tr[aria-current]').length };
      });
      P.runs.paints = markerPaints;
      P.runs.controls = controlsOfRuns;
      await page.close();
    }

  }
  // A request and a choice: more contrast beside a theme stored by the reader (their choice wins) and
  // beside a host's default (the request wins), and the host's default alone.
  const combos = {
    'prefers-contrast+stored': { page: 'run', theme: 'dark', features: [{ name: 'prefers-contrast', value: 'more' }] },
    'prefers-contrast+default': { page: 'runDefault', theme: null, features: [{ name: 'prefers-contrast', value: 'more' }] },
    'default': { page: 'runDefault', theme: null },
    // A locked page: the one theme whatever the system prefers or a script sets in the page, the
    // switch hidden, and the system's requests for more contrast and forced colors still win.
    'locked': { page: 'runLocked', theme: null },
    'locked+system-light': { page: 'runLocked', theme: null, features: [{ name: 'prefers-color-scheme', value: 'light' }] },
    'locked-light+system-dark': { page: 'runLockedLight', theme: null, features: [{ name: 'prefers-color-scheme', value: 'dark' }] },
    'locked+set-in-page': { page: 'runLocked', theme: 'light' },
    'locked+prefers-contrast': { page: 'runLocked', theme: null, features: [{ name: 'prefers-contrast', value: 'more' }] },
    'locked+prefers-contrast+set-in-page': { page: 'runLocked', theme: 'light', features: [{ name: 'prefers-contrast', value: 'more' }] },
    'locked+forced-colors': { page: 'runLocked', theme: null, features: [{ name: 'forced-colors', value: 'active' }] },
    'locked+forced-colors+set-in-page': { page: 'runLocked', theme: 'light', features: [{ name: 'forced-colors', value: 'active' }] },
  };
  out.combos = {};
  for (const [key, c] of Object.entries(combos)) {
    mode = c;
    const page = await open(c.page);
    out.combos[key] = await page.evaluate(() => ({ bg: window.__bg, fg: window.__fg, bgToken: window.__bgToken, current: window.bideTheme.current(), switchDisplay: getComputedStyle(document.getElementById('theme')).display, names: window.bideTheme.names().join(' ') }));
    await page.close();
  }
  out.ok = true;
} catch (e) {
  out.error = String((e && e.message) || e);
}
await browser.close();
console.log(JSON.stringify(out));
