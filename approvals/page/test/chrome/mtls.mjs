// mtls.mjs: drives the approve page in real Chrome for the mutual-TLS check
// (approvals/page_mtls_test.go, which starts the servers, puts a client certificate in the profile's
// NSS database and runs this). It opens the page at PAGE_URL, creates the approver's key, hands the
// public key to the Go side through ENROL_DIR (pub, then waits for enrolled), types the approver id,
// presses Approve and prints one JSON line: what the page said, and what the browser did.
import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const env = process.env;
const log = [];
const note = (s) => log.push(s);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await puppeteer.launch({
  executablePath: env.CHROME_BIN,
  userDataDir: env.BIDE_CHROME_PROFILE,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  timeout: 60000,
});
let out;
try {
  const page = await browser.newPage();
  page.on('console', (m) => note(`console ${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => note(`pageerror: ${e.message}`));
  page.on('requestfailed', (r) => note(`requestfailed: ${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  page.on('response', (r) => note(`response: ${r.request().method()} ${r.url()} ${r.status()}`));
  const text = (id) => page.$eval(`#${id}`, (e) => e.textContent);
  const stage = async (name, fn) => {
    try {
      return await fn();
    } catch (e) {
      throw new Error(`${name}: ${e.message}; status "${await text('status').catch(() => '?')}"`);
    }
  };

  await stage('open the page', () => page.goto(env.PAGE_URL, { waitUntil: 'load', timeout: 30000 }));
  // The call loads with the page (a GET /pending with the page's credentials mode), or the page says
  // why it could not.
  await stage('the call loads', () => page.waitForFunction(() => !document.getElementById('call-panel').hidden || (document.getElementById('status').textContent !== '' && document.getElementById('status').textContent !== 'Loading the call...'), { timeout: 30000 }));
  if (await page.$eval('#call-panel', (e) => e.hidden)) {
    out = { ok: false, loaded: false, status: await text('status'), log };
    throw out;
  }
  const tool = await text('call-tool');
  const origin = await text('origin');
  await stage('create the key', async () => {
    await page.waitForSelector('#create-key:not([hidden])', { timeout: 10000 });
    await page.click('#create-key');
    await page.waitForFunction(() => document.getElementById('key-public').textContent.startsWith('ed25519:'), { timeout: 10000 });
  });
  const pub = await text('key-public');
  fs.writeFileSync(path.join(env.ENROL_DIR, 'pub.tmp'), pub);
  fs.renameSync(path.join(env.ENROL_DIR, 'pub.tmp'), path.join(env.ENROL_DIR, 'pub'));
  await stage('wait for the enrolment', async () => {
    for (let i = 0; i < 300 && !fs.existsSync(path.join(env.ENROL_DIR, 'enrolled')); i++) await sleep(100);
    if (!fs.existsSync(path.join(env.ENROL_DIR, 'enrolled'))) throw new Error('the Go side did not enrol the key');
  });
  await stage('approve', async () => {
    await page.type('#approver', env.APPROVER);
    await page.waitForSelector('#approve:not([disabled])', { timeout: 10000 });
    await page.click('#approve');
    await page.waitForFunction(() => { const t = document.getElementById('result').textContent; return t !== '' && !t.startsWith('Signing'); }, { timeout: 30000 });
  });
  const result = await text('result');
  const cls = await page.$eval('#result', (e) => e.className);
  out = { ok: cls === 'ok', loaded: true, result, tool, origin, log };
} catch (e) {
  if (e !== out) out = { ok: false, error: e.message, log };
} finally {
  await browser.close();
}
console.log(JSON.stringify(out));
