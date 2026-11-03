// build.mjs [OUT.png]: render results.json into assets/chaos-results.png.
// Needs Node 20+, puppeteer-core (assets/demo/browser-shots: npm ci --ignore-scripts),
// a Chrome (CHROME_BIN) and python3 with Pillow (assets/demo/optimize-png.py).
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(path.join(here, '../browser-shots/package.json'));
const puppeteer = require('puppeteer-core');
const out = path.resolve(process.argv[2] ?? path.join(here, '../../chaos-results.png'));
const data = JSON.parse(fs.readFileSync(path.join(here, 'results.json'), 'utf8'));
const html = fs.readFileSync(path.join(here, 'card.html'), 'utf8');

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'chaos-card-'));
const page_ = path.join(tmp, 'card.html');
fs.writeFileSync(page_, html.replace('<script>', `<script>window.RESULTS=${JSON.stringify(data)};</script>\n<script>`));
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run'] });
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1200, height: 1280, deviceScaleFactor: 1 });
  await page.goto('file://' + page_, { waitUntil: 'load' });
  const raw = path.join(tmp, 'raw.png');
  await page.screenshot({ path: raw });
  execFileSync('python3', [path.join(here, '../optimize-png.py'), raw, out]);
} finally { await browser.close(); fs.rmSync(tmp, { recursive: true, force: true }); }
console.log('wrote', out);
