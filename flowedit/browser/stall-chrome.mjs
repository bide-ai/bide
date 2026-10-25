// stall-chrome.mjs: launches Chrome the way every script here does (in the run's profile directory),
// opens a page, says so, and waits for ever. TestScriptsHaveADeadlineInChrome runs it to check that the Chrome
// a stalled script started is gone after the deadline. With STALL=busy the script then spins in a loop
// that never returns to the event loop, so puppeteer's SIGTERM handler cannot run and only the harness
// killing the Chrome by its profile directory can remove it.
import puppeteer from 'puppeteer-core';

const env = process.env;
const browser = await puppeteer.launch({ executablePath: env.CHROME_BIN, userDataDir: env.BIDE_CHROME_PROFILE, headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage'], timeout: 60000 });
const page = await browser.newPage();
await page.goto('about:blank');
// What Chrome was really started with, not what the environment says it should have been.
const used = browser.process().spawnargs.find((a) => a.startsWith('--user-data-dir='))?.slice('--user-data-dir='.length);
console.log('launched ' + used);
if (env.STALL === 'busy') { for (;;) { /* a stuck event loop */ } }
setInterval(() => {}, 1 << 30);
