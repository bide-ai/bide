// app.js: the browser page. It holds one Ed25519 key per origin as a non-extractable WebCrypto key
// in IndexedDB, shows a pending call from the intake, and signs a decision on it. Everything shown
// is set with textContent, never as markup. The logic that decides what is signed is in flow.js and
// core.js, which the tests run against a real endpoint; this file is the glue to the document.

import { argsNonEmpty, keyFingerprint, publicKeyText, prettyArgs, reasonLine, safeText } from './core.js';
import { Refused, decide, loadCall } from './flow.js';

const $ = (id) => document.getElementById(id);
const text = (id, s) => {
  $(id).textContent = s;
};

const DB = 'bide-approve';
const STORE = 'keys';
const KEY_NAME = 'approver-key';

function idb() {
  return new Promise((resolve, reject) => {
    const r = indexedDB.open(DB, 1);
    r.onupgradeneeded = () => r.result.createObjectStore(STORE);
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}
async function idbGet() {
  const db = await idb();
  return new Promise((resolve, reject) => {
    const q = db.transaction(STORE).objectStore(STORE).get(KEY_NAME);
    q.onsuccess = () => resolve(q.result);
    q.onerror = () => reject(q.error);
  });
}
async function idbPut(v) {
  const db = await idb();
  return new Promise((resolve, reject) => {
    const q = db.transaction(STORE, 'readwrite').objectStore(STORE).put(v, KEY_NAME);
    q.onsuccess = () => resolve();
    q.onerror = () => reject(q.error);
  });
}

async function ed25519Supported() {
  try {
    const k = await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify']);
    return !!k.privateKey;
  } catch {
    return false;
  }
}

let key = null; // {privateKey, publicKey, publicRaw}
let shown = null; // loadCall's result for the call on display
let base = null; // the intake's base URL
let credentials = 'same-origin'; // fetch's credentials mode (config.json may say "include")
let scrolledToEnd = true;

async function intakeBase() {
  // A deployment that hosts this page on an origin of its own names the intake in config.json next to
  // it; otherwise the page was served by the application, at <mount>/page/, and the intake is <mount>/.
  try {
    const r = await fetch(new URL('config.json', location.href), { cache: 'no-store', credentials: 'omit' });
    if (r.ok) {
      const c = await r.json();
      const u = new URL(c.intake);
      if (u.protocol === 'https:' || (u.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(u.hostname))) {
        if (!u.pathname.endsWith('/')) u.pathname += '/';
        return { url: u, hosted: true, credentials: ['same-origin', 'include', 'omit'].includes(c.credentials) ? c.credentials : 'same-origin' };
      }
    }
  } catch {
    // no config: served by the application
  }
  return { url: new URL('../', location.href), hosted: false, credentials: 'same-origin' };
}

function fragment() {
  const p = new URLSearchParams(location.hash.slice(1));
  return { run: p.get('run') ?? '', call: p.get('call') ?? '' };
}

async function showKey() {
  const fp = await keyFingerprint(key.publicRaw);
  text('key-public', publicKeyText(key.publicRaw));
  text('key-fingerprint', fp.grouped);
  $('key-panel').hidden = false;
  $('create-key').hidden = true;
}

async function createKey() {
  const pair = await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify']);
  const publicRaw = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey));
  await idbPut({ privateKey: pair.privateKey, publicKey: pair.publicKey, publicRaw });
  key = { ...pair, publicRaw };
  await showKey();
  refresh();
}

function renderCall() {
  const f = shown.fields;
  text('call-run', safeText(shown.subject.run));
  text('call-call', safeText(shown.subject.call));
  text('call-tool', safeText(shown.subject.tool));
  text('call-asked', f.asked_at ? safeText(f.asked_at) : 'not recorded');
  const g = f.gate;
  text('gate-need', `${g.need} of ${g.approvers.length} must approve: ${safeText(g.approvers.join(', '))}`);
  text('gate-decided', (g.decisions ?? []).filter((d) => d.counted).map((d) => `${safeText(d.approver)} ${d.approved ? 'approved' : 'denied'}`).join('; ') || 'nobody yet');
  text('gate-pending', safeText((g.pending ?? []).join(', ')) || 'nobody');
  text('call-state', safeText(f.state));
  // All of the arguments, always: the signature binds every one of them, so none is cut from what the
  // approver reads. A long value scrolls in its box.
  text('call-args', prettyArgs(shown.args));
  // A string in the arguments is something the model wrote: said so, once, below them.
  $('args-note').hidden = !argsHaveText(shown.args);
  // The reasons, as text: each is a line set with textContent, never markup. They are the application's claim.
  const list = $('why-list');
  list.replaceChildren();
  for (const r of f.why ?? []) {
    const li = document.createElement('li');
    li.textContent = safeText(reasonLine(r)) + (r.at ? ` (as of ${safeText(r.at)})` : '');
    if (r.kind === 'conflict') li.className = 'warn';
    list.append(li);
  }
  $('why-panel').hidden = (f.why ?? []).length === 0;
  text('call-digest', shown.subject.argsHex);
  text('call-reason', f.reason ? safeText(f.reason) : '');
  $('call-reason').hidden = $('call-reason-label').hidden = !f.reason;
  $('call-panel').hidden = false;
  const box = $('call-args');
  scrolledToEnd = box.scrollHeight <= box.clientHeight + 2;
}

function refresh() {
  const approver = $('approver').value;
  let why = '';
  if (!shown) why = 'No call is loaded.';
  else if (!key) why = 'Create a key first.';
  else if (!scrolledToEnd) why = 'Scroll to the end of the arguments first (an aid, not a control).';
  else if (!shown.fields.gate.approvers.includes(approver)) why = 'Type your approver id, exactly as the gate lists it.';
  else {
    const p = shown.fields.state === 'waiting' ? null : `The call is ${safeText(shown.fields.state)}.`;
    if (p) why = p;
  }
  const ok = why === '';
  $('approve').disabled = !ok;
  $('reject').disabled = !ok;
  text('why', why);
}

async function submit(approved) {
  $('approve').disabled = $('reject').disabled = true;
  text('result', 'Signing and sending...');
  $('result-panel').hidden = false;
  $('result').className = '';
  try {
    const out = await decide({
      fetchFn: (u, o) => fetch(u, o),
      base: base.url,
      shown,
      approver: $('approver').value,
      approved,
      sign: async (msg) => new Uint8Array(await crypto.subtle.sign('Ed25519', key.privateKey, msg)),
      publicKey: { verify: (msg, sig) => crypto.subtle.verify('Ed25519', key.publicKey, sig, msg) },
      credentials,
    });
    const g = out.response.gate;
    text('result', `${approved ? 'Approved' : 'Rejected'}: signed and recorded by the application. The gate (as the application counts it): ${g.approved} of ${g.need} needed approved, ${g.denied} denied.${g.passed ? ' The gate has its approvals.' : ''}`);
    $('result').className = 'ok';
    offerReceipt(out.receipt);
  } catch (e) {
    text('result', e instanceof Refused ? e.message : `Failed: ${e.message}`);
    $('result').className = 'bad';
  }
}

function offerReceipt(rc) {
  const a = $('receipt');
  const blob = new Blob([JSON.stringify(rc, null, 2) + '\n'], { type: 'application/json' });
  a.href = URL.createObjectURL(blob);
  a.download = `${shown.subject.run}-${shown.subject.call}-${$('approver').value}.receipt.json`.replace(/[^A-Za-z0-9._-]/g, '_');
  a.hidden = false;
}

async function load() {
  const { run, call } = fragment();
  if (!run || !call) {
    text('status', 'Open the link from the notification: it names the run and the call.');
    return;
  }
  text('status', 'Loading the call...');
  try {
    shown = await loadCall((u, o) => fetch(u, o), base.url, run, call, credentials);
    text('status', '');
    renderCall();
  } catch (e) {
    shown = null;
    text('status', e instanceof Refused ? e.message : `Failed: ${e.message}`);
  }
  refresh();
}

async function main() {
  if (!(await ed25519Supported())) {
    text('status', 'This browser does not support Ed25519 in WebCrypto, so it cannot sign here. Use a current Chrome, Firefox or Safari, or bide approve.');
    return;
  }
  const b = await intakeBase();
  base = b;
  credentials = b.credentials;
  text('origin', b.hosted
    ? `This page is hosted at ${location.origin} and talks to ${b.url.origin}.`
    : `This page was served by the application at ${location.origin}: the application can change what it does. Use a page hosted by your organization for anything that matters.`);
  $('origin').className = b.hosted ? '' : 'warn';
  const stored = await idbGet().catch(() => undefined);
  if (stored) {
    key = stored;
    await showKey();
  } else {
    $('create-key').hidden = false;
  }
  $('create-key').addEventListener('click', createKey);
  $('approver').addEventListener('input', refresh);
  $('approve').addEventListener('click', () => submit(true));
  $('reject').addEventListener('click', () => submit(false));
  $('call-args').addEventListener('scroll', () => {
    const el = $('call-args');
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 2) {
      scrolledToEnd = true;
      refresh();
    }
  });
  addEventListener('hashchange', load);
  // The browser enforces the host's Content-Security-Policy: an intake origin that is not in its
  // connect-src (a config.json that was changed to name another one) is blocked, and said so.
  document.addEventListener('securitypolicyviolation', (e) => {
    text('status', `The browser blocked a request to ${safeText(e.blockedURI)} (the policy this page was served under allows only the intake it was deployed for): the page's config.json may have been changed.`);
  });
  await load();
}

main().catch((e) => text('status', `Failed to start: ${e.message}`));
