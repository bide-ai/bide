// spoof.mjs: names with characters that do not print (a hand-written file can have them; the loader
// accepts any string) are shown escaped, never drawn, on every surface of the page. In each state
// (opened, each node selected, the remove dialog, the weakened banner with the save dialog, the
// conflict dialog) it walks every text node, every title, aria-label, placeholder and alt, and every
// option's text, outside the source pane and the inputs an author edits in, and reports each
// character that does not print. It prints one JSON line.
import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const env = process.env;
const console_ = [];
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => console_.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => console_.push(`pageerror: ${e.message}`));
  await page.setViewport({ width: 1400, height: 1600 });
  await page.goto(env.PAGE_URL, { waitUntil: 'networkidle0', timeout: 30000 });
  await page.waitForSelector('#outline button');
  const until = (fn, what) => page.waitForFunction(fn, { timeout: 10000 }).catch(() => { throw new Error('timed out waiting for ' + what); });
  const hits = [];
  const walk = async (state) => {
    const found = await page.evaluate(() => {
      const bad = /[^\p{L}\p{M}\p{N}\p{P}\p{S} \t\n\r]/u;
      const res = [];
      const tw = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      for (let n = tw.nextNode(); n; n = tw.nextNode()) {
        const p = n.parentElement;
        if (!p || p.closest('#source, script, style')) continue;
        let t = n.textContent;
        if (p.id === 'live') t = t.replace(/ $/, ''); // announce() alternates a no-break space on a repeat
        const m = bad.exec(t);
        if (m) res.push({ where: p.tagName.toLowerCase() + (p.id ? '#' + p.id : '') + (p.getAttribute('class') ? '.' + p.getAttribute('class') : ''), text: t.slice(0, 80), cp: m[0].codePointAt(0).toString(16) });
      }
      for (const e of document.body.querySelectorAll('*')) {
        for (const a of ['aria-label', 'title', 'placeholder', 'alt', 'aria-description']) {
          const v = e.getAttribute(a);
          if (v && bad.test(v)) res.push({ where: e.tagName.toLowerCase() + '[' + a + ']', text: v.slice(0, 80), cp: bad.exec(v)[0].codePointAt(0).toString(16) });
        }
      }
      return res;
    });
    for (const f of found) hits.push({ state, ...f });
  };
  const r = {};
  r.outline = await page.$$eval('#outline button .nm', (b) => b.map((x) => x.textContent));
  r.title = '';
  await walk('opened');
  const n = r.outline.length;
  for (let i = 0; i < n; i++) {
    await page.evaluate((i) => document.querySelectorAll('#outline button')[i].click(), i);
    await until(() => document.getElementById('f-name'), 'the inspector');
    await walk('node ' + i + ' selected');
    if (i === 0) {
      r.title = await page.$eval('#inspector h3', (e) => e.textContent);
      r.warning = await page.$eval('#f-name-escaped', (e) => e.textContent);
      r.nameField = await page.$eval('#f-name', (e) => e.value);
    }
  }
  // The remove dialog of the second node (edges name it, so the dialog lists them).
  await page.evaluate(() => document.querySelectorAll('#outline button')[1].click());
  await until(() => document.getElementById('f-remove'), 'the remove button');
  await page.click('#f-remove');
  await until(() => document.getElementById('dialog').open && document.getElementById('remove-list'), 'the remove dialog');
  await walk('remove dialog');
  await page.keyboard.press('Escape');
  await until(() => !document.getElementById('dialog').open, 'the dialog to close');
  // A weakened text: the gate is removed in the source. The banner, the verdict and the save dialog.
  await page.$eval('#source', (e) => { e.value = e.value.replace(/,"approval":\s*\{[^}]*\}/, ''); e.dispatchEvent(new Event('input', { bubbles: true })); });
  await until(() => document.getElementById('weakened')?.textContent.includes('WEAKENED'), 'the weakened banner');
  await walk('weakened banner');
  await page.click('#save');
  await until(() => document.getElementById('dialog').open && document.getElementById('confirm-weaken'), 'the save dialog');
  await walk('save dialog');
  // The file changes on disk behind the editor: the save answers with the conflict dialog.
  fs.appendFileSync(env.FILE, '\n');
  await page.click('#confirm-weaken');
  await page.click('#do-save');
  await until(() => /changed on disk/i.test(document.getElementById('dialog-title')?.textContent || ''), 'the conflict dialog');
  await walk('conflict dialog');
  out = { ok: true, ...r, hits, console: console_ };
} catch (e) {
  out = { ok: false, error: String(e && e.message || e), console: console_ };
}
await browser.close();
console.log(JSON.stringify(out));
process.exit(out.ok ? 0 : 1);
