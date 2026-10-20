// colours.mjs: the approve page in real Chrome under each way the system can ask for a look: light, dark,
// more contrast and forced colors. For each it reports what the browser computed (not what the
// stylesheet says): the root's --bg custom property (which theme block won), the text of the page against
// what it sits on, the edge of every control against its surroundings, whether a disabled button is
// told by its shape, and the focus ring. approvals/page_chrome_test.go judges the result. The page is
// served by a real intake with a call pending (PAGE_URL), so the call panel and its controls are drawn.
import puppeteer from 'puppeteer-core';

const env = process.env;
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
const out = { ok: false, modes: {}, console: [] };

const measure = () => {
  const rgb = (c) => { const m = (c || '').match(/rgba?\(([^)]*)\)/); if (!m) return null; const p = m[1].split(/[,\s/]+/).filter(Boolean).map(parseFloat); return [p[0], p[1], p[2], p.length > 3 ? p[3] : 1]; };
  const bgOf = (el) => { for (let e = el; e && e.nodeType === 1; e = e.parentElement) { const c = rgb(getComputedStyle(e).backgroundColor); if (c && c[3] >= 0.5) return c.slice(0, 3); } return [255, 255, 255]; };
  const shown = (e) => { for (let x = e; x && x.nodeType === 1; x = x.parentElement) { const s = getComputedStyle(x); if (s.display === 'none' || s.visibility === 'hidden') return false; } return e.getClientRects().length > 0; };
  const texts = [];
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const e = n.parentElement;
    if (!n.textContent.trim() || !shown(e) || e.tagName === 'SCRIPT' || e.tagName === 'STYLE') continue;
    const s = getComputedStyle(e), c = rgb(s.color);
    texts.push({ tag: e.tagName.toLowerCase(), id: e.id, cls: e.className, text: n.textContent.trim().slice(0, 24), color: c.slice(0, 3), alpha: c[3], bg: bgOf(e), disabled: !!e.closest(':disabled'), size: parseFloat(s.fontSize) });
  }
  const chainOpacity = (el) => { let o = 1; for (let e = el; e && e.nodeType === 1; e = e.parentElement) o *= parseFloat(getComputedStyle(e).opacity); return o; };
  const probe = (css) => { const d = document.createElement('span'); d.style.backgroundColor = css; document.body.appendChild(d); const c = getComputedStyle(d).backgroundColor; d.remove(); return c; };
  // How an element is drawn: visible in the page (size, place, opacity up the tree, font size), and what
  // its label is made of: the DOM text, and the text its ::before and ::after add (CSS content).
  const drawn = (e) => {
    const s = getComputedStyle(e), r = e.getBoundingClientRect(), pb = getComputedStyle(e, '::before').content, pa = getComputedStyle(e, '::after').content;
    return { w: r.width, h: r.height, left: r.left, top: r.top + window.scrollY, right: r.right, vw: document.documentElement.clientWidth, opacity: chainOpacity(e), fontSize: parseFloat(s.fontSize), shown: shown(e), text: e.textContent.trim().slice(0, 40), before: pb, after: pa, transform: s.transform, clip: s.clipPath, overflow: s.overflow };
  };
  const controls = [...document.querySelectorAll('input, button')].filter(shown).map((e) => {
    const s = getComputedStyle(e), bc = rgb(s.borderTopColor) || [0, 0, 0, 0], fc = rgb(s.backgroundColor) || [0, 0, 0, 0];
    return { tag: e.tagName.toLowerCase(), id: e.id, cls: e.className, disabled: e.disabled, borderWidth: parseFloat(s.borderTopWidth), borderStyle: s.borderTopStyle, border: bc.slice(0, 3), borderAlpha: bc[3], fill: fc.slice(0, 3), fillAlpha: fc[3], parentBG: bgOf(e.parentElement), drawn: drawn(e) };
  });
  // The things an approver must see, by id: the origin line, the call's fields, the digest.
  const must = {};
  for (const id of ['origin', 'call-run', 'call-tool', 'call-state', 'gate-need', 'call-args', 'call-digest', 'approve', 'reject', 'approver']) {
    const e = document.getElementById(id);
    must[id] = e ? drawn(e) : null;
  }
  // The buttons' colors by role: Approve is the ok color and REJECT the alert one (a swap is a lie), read
  // as the browser resolves the theme's custom properties.
  const roles = { good: probe('var(--good)'), bad: probe('var(--bad)') };
  // The two buttons as the page draws them once a key is enrolled and an approver typed: enabled. They
  // are disabled until then, so the measurement enables them for a moment and puts them back.
  const enabled = {};
  for (const id of ['approve', 'reject']) {
    const b = document.getElementById(id), was = b.disabled;
    b.disabled = false;
    const s = getComputedStyle(b), bc = rgb(s.borderTopColor), fc = rgb(s.backgroundColor), tc = rgb(s.color);
    enabled[id] = { borderWidth: parseFloat(s.borderTopWidth), borderStyle: s.borderTopStyle, border: bc.slice(0, 3), fill: fc.slice(0, 3), fillAlpha: fc[3], text: tc.slice(0, 3), parentBG: bgOf(b.parentElement), drawn: drawn(b), fillCSS: s.backgroundColor, borderCSS: s.borderTopColor };
    b.disabled = was;
  }
  // The result panel is drawn only after a signature; draw it as the page would, ok and bad, to see
  // its text. The panel is put back as it was.
  const panel = document.getElementById('result-panel'), result = document.getElementById('result'), wasHidden = panel.hidden, wasClass = result.className, wasText = result.textContent;
  const results = {};
  panel.hidden = false;
  for (const [cls, text] of [['ok', 'Counted: your approval is recorded.'], ['bad', 'Refused: the endpoint did not accept the decision.']]) {
    result.className = cls === 'ok' ? 'ok' : 'bad';
    result.textContent = text;
    const s = getComputedStyle(result), c = rgb(s.color);
    results[cls] = { color: c.slice(0, 3), alpha: c[3], bg: bgOf(result), drawn: drawn(result), weight: s.fontWeight };
  }
  panel.hidden = wasHidden; result.className = wasClass; result.textContent = wasText;
  // The focus ring: put the focus on the approver field and read the outline.
  const input = document.getElementById('approver');
  input.focus();
  const fs = getComputedStyle(input);
  const focus = { style: fs.outlineStyle, width: parseFloat(fs.outlineWidth), color: (rgb(fs.outlineColor) || [0, 0, 0]).slice(0, 3), bg: bgOf(input.parentElement) };
  input.blur();
  const root = getComputedStyle(document.documentElement);
  return { token: root.getPropertyValue('--bg').trim(), fgToken: root.getPropertyValue('--fg').trim(), texts, controls, focus, must, roles, enabled, results, bodyBG: getComputedStyle(document.body).backgroundColor };
};

try {
  const modes = {
    'light': [{ name: 'prefers-color-scheme', value: 'light' }],
    'dark': [{ name: 'prefers-color-scheme', value: 'dark' }],
    'prefers-contrast': [{ name: 'prefers-color-scheme', value: 'dark' }, { name: 'prefers-contrast', value: 'more' }],
    'forced-colors': [{ name: 'prefers-color-scheme', value: 'dark' }, { name: 'forced-colors', value: 'active' }],
  };
  for (const [name, features] of Object.entries(modes)) {
    const page = await browser.newPage();
    // The page asks for config.json (optional) and the browser for a favicon; both may be 404. Any other
    // failed response, and any console error that is not that 404 itself, is reported.
    page.on('response', (r) => { if (r.status() >= 400 && !/\/(config\.json|favicon\.ico)$/.test(new URL(r.url()).pathname)) out.console.push(name + ': ' + r.status() + ' ' + r.url()); });
    page.on('console', (m) => { if ((m.type() === 'error' || m.type() === 'warning') && !/status of 404/.test(m.text())) out.console.push(name + ': ' + m.text()); });
    page.on('pageerror', (e) => out.console.push(name + ': ' + e.message));
    await page.emulateMediaFeatures(features);
    await page.setViewport({ width: 900, height: 1400 });
    await page.goto(env.PAGE_URL, { waitUntil: 'load', timeout: 30000 });
    await page.waitForSelector('#call-panel:not([hidden])', { timeout: 15000 });
    out.modes[name] = await page.evaluate(measure);
    await page.close();
  }
  out.ok = true;
} catch (e) {
  out.error = String((e && e.message) || e);
}
await browser.close();
console.log(JSON.stringify(out));
