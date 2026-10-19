// colouronly.mjs: loads bide view's pages in real Chrome and reports, for each state the audit in
// docs/design/themes.md says must not differ by colour alone, what the browser computed: the dash
// pattern, width and cap of each edge, the stroke dash and width of each node box, the shape of each
// node mark, the glyph a label or a row gets, and the badge a new row gets. Computed styles, not CSS
// text: the cascade, specificity and whitespace are the browser's. The Go test
// (internal/runview/browser_test.go) judges what this prints.
//
// PAGES is a JSON object of page name to URL: config, diff, run, runs. Prints one JSON line.
import puppeteer from 'puppeteer-core';

const env = process.env;
const pages = JSON.parse(env.PAGES);
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
const out = { ok: false, pages: {}, console: [] };
try {
  const open = async (name) => {
    const page = await browser.newPage();
    page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') out.console.push(name + ': ' + m.text()); });
    page.on('pageerror', (e) => out.console.push(name + ': ' + e.message));
    await page.setViewport({ width: 1400, height: 1000 });
    await page.goto(pages[name], { waitUntil: 'networkidle0', timeout: 30000 });
    return page;
  };

  // The edges and node states a graph page draws, as the browser computed them.
  const graph = () => {
    const edges = [...document.querySelectorAll('svg.graph path.edge')].map((e) => {
      const s = getComputedStyle(e);
      return { classes: [...e.classList].filter((c) => c !== 'back'), dash: s.strokeDasharray, width: parseFloat(s.strokeWidth), cap: s.strokeLinecap };
    });
    const nodes = [...document.querySelectorAll('svg.graph g.node')].map((n) => {
      const box = n.querySelector('.box');
      const s = getComputedStyle(box);
      const mark = n.querySelector('.nmark');
      return { classes: [...n.classList], dash: s.strokeDasharray, width: parseFloat(s.strokeWidth), mark: mark ? { shape: mark.firstElementChild.tagName.toLowerCase(), text: mark.querySelector('text').textContent } : null };
    });
    return { edges, nodes };
  };

  for (const name of ['config', 'diff', 'run']) {
    const page = await open(name);
    out.pages[name] = await page.evaluate(graph);
    if (name === 'run') {
      // Stats: the three colored judgements, as the page draws them (one of each, appended).
      out.pages.run.stats = await page.evaluate(() => {
        const ul = document.querySelector('.stats') || document.querySelector('main').appendChild(document.createElement('ul'));
        const got = {};
        for (const c of ['good', 'bad', 'warn']) {
          const li = document.createElement('li');
          li.className = 'stat s-' + c;
          li.innerHTML = '<span class="num">1</span><span class="lbl">label</span>';
          ul.appendChild(li);
          got[c] = getComputedStyle(li.querySelector('.lbl'), '::before').content;
        }
        return got;
      });
      // New timeline rows: the live script's marking, run on this page's own rows.
      await page.addStyleTag({ url: '/assets/live.css' });
      await page.addScriptTag({ url: '/assets/live.js' });
      out.pages.run.fresh = await page.evaluate(() => {
        const rows = [...document.querySelectorAll('.timeline > li')];
        window.bideMarkFresh(rows, 1);
        return rows.map((r, i) => {
          const b = r.querySelector('.newbadge');
          return { index: i, fresh: r.classList.contains('fresh'), badge: b ? b.textContent : null, badgeVisible: b ? getComputedStyle(b).display !== 'none' && b.getBoundingClientRect().width > 0 : false };
        });
      });
    }
    await page.close();
  }

  // The run list: j puts the keyboard on a row; it is marked by a glyph and aria-current.
  {
    const page = await open('runs');
    await page.keyboard.press('j');
    out.pages.runs = await page.evaluate(() => {
      const r = document.querySelector('table.runlist tbody tr.cur');
      if (!r) return { cur: false };
      return { cur: true, ariaCurrent: r.getAttribute('aria-current'), glyph: getComputedStyle(r.querySelector('td:first-child'), '::before').content, others: [...document.querySelectorAll('table.runlist tbody tr[aria-current]')].length };
    });
    await page.close();
  }
  out.ok = true;
} catch (e) {
  out.error = String((e && e.message) || e);
}
await browser.close();
console.log(JSON.stringify(out));
