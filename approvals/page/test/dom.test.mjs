// dom.test.mjs: the shipped script (page.js: the modules joined, browser glue included) run against a
// minimal DOM and a stub endpoint. PAGE_JS names the built script; the Go tests build it and set it.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { createHash } from 'node:crypto';
import { canonicalArgs } from '../canon.js';

const script = fs.readFileSync(process.env.PAGE_JS ?? '', 'utf8');

function el() {
  const listeners = {};
  // Markup is never assigned: a page that sets innerHTML, outerHTML or inserts HTML fails here.
  const trap = (name) => ({ set() { throw new Error(`${name} was assigned: the page must set text, not markup`); }, get() { return ''; } });
  const e = { children: [], replaceChildren() { this.children = []; }, append(c) { this.children.push(c); }, textContent: '', hidden: false, disabled: false, value: '', dataset: {}, className: '', href: '', scrollHeight: 0, clientHeight: 100, scrollTop: 0,
    addEventListener(t, f) { (listeners[t] ??= []).push(f); }, fire(t) { return Promise.all((listeners[t] ?? []).map((f) => f())); } };
  Object.defineProperty(e, 'innerHTML', trap('innerHTML'));
  Object.defineProperty(e, 'outerHTML', trap('outerHTML'));
  return e;
}

// until polls cond every few milliseconds, failing with what it waited for after ten seconds.
async function until(cond, what) {
  for (let i = 0; i < 2000; i++) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 5));
  }
  throw new Error(`timed out waiting for ${what}`);
}

// run loads the page against an endpoint that answers pendingBody, and returns the elements.
async function run(pendingBody, { href = 'https://app.example/approvals/page/', config = null, delay = 0 } = {}) {
  const els = new Proxy({}, { get: (t, k) => (t[k] ??= el()) });
  els['call-panel'].hidden = true; // as in the page's markup: it is shown when the call has loaded
  const store = {};
  const indexedDB = { open() {
    const req = {};
    queueMicrotask(() => { req.result = { createObjectStore() {}, transaction() { return { objectStore: () => ({
      get: () => { const q = {}; queueMicrotask(() => { q.result = store.k; q.onsuccess?.(); }); return q; },
      put: (v) => { store.k = v; const q = {}; queueMicrotask(() => q.onsuccess?.()); return q; } }) }; } };
      req.onupgradeneeded?.(); req.onsuccess?.(); });
    return req; } };
  const fetched = [];
  const docListeners = {};
  const ctx = { document: { getElementById: (id) => els[id], createElement: () => el(), addEventListener: (t, f) => { docListeners[t] = f; } }, indexedDB, crypto: globalThis.crypto, TextEncoder, TextDecoder, URL, URLSearchParams, Blob, btoa, atob, Uint8Array,
    location: { href, hash: '#run=r1&call=c1', origin: new URL(href).origin },
    fetch: async (u, o) => {
      fetched.push({ url: String(u), opts: o });
      if (String(u).endsWith('config.json')) return config ? { ok: true, json: async () => config } : { ok: false };
      if (delay) await new Promise((r) => setTimeout(r, delay)); // a slow endpoint, or a slow CI runner
      return { ok: true, status: 200, arrayBuffer: async () => new TextEncoder().encode(pendingBody).buffer };
    },
    addEventListener() {}, console, Promise, Array, Map, Set, Math, Date, JSON, Error, Object, String, Number, Symbol, Function, setTimeout, queueMicrotask };
  vm.createContext(ctx);
  vm.runInContext(script, ctx);
  // Wait for the page to finish loading the call (it shows the panel, or says why not), not for a fixed
  // time: the load is asynchronous (IndexedDB, two fetches, SHA-256), and a slow runner is slower.
  await until(() => !els['call-panel'].hidden || !['', 'Loading the call...'].includes(els['status'].textContent), 'the page to load the call');
  await els['create-key'].fire('click');
  els['approver'].value = 'ops';
  await els['approver'].fire('input'); // fire resolves when the handlers have, so nothing is left to sleep for
  return { els, fetched, docListeners };
}

const pendingWith = (args, over = {}) => {
  const a = typeof args === 'string' ? args : JSON.stringify(args);
  const digest = createHash('sha256').update(canonicalArgs(new TextEncoder().encode(a))).digest('hex');
  const p = { run: 'r1', call: 'c1', tool: 'wire', args: '@@', args_sha256: digest, decision_format: 'bide.approval.v3', state: 'waiting',
    gate: { need: 2, approvers: ['ops', 'fin', 'risk'], approved: 0, denied: 0, pending: ['ops', 'fin', 'risk'], passed: false, failed: false }, ...over };
  return JSON.stringify(p).replace('"@@"', a);
};

test('the harness waits for the page to load the call, however long the endpoint takes (no fixed sleep)', async () => {
  const { els } = await run(pendingWith({ a: 1, zz_destination: 'ATTACKER-ACCT-9999' }), { delay: 600 });
  assert.ok(els['call-args'].textContent.includes('ATTACKER-ACCT-9999'));
});

test('every one of the arguments is on display, so Approve never signs what was cut from view', async () => {
  const lines = {};
  for (let i = 0; i < 80; i++) lines['k' + String(i).padStart(2, '0')] = i;
  lines.zz_destination = 'ATTACKER-ACCT-9999';
  lines.long = 'x'.repeat(5000) + 'TAIL-OF-A-LONG-VALUE';
  const { els } = await run(pendingWith(lines));
  const shown = els['call-args'].textContent;
  assert.ok(shown.includes('ATTACKER-ACCT-9999'), 'the decisive field below the old cut is on display');
  assert.ok(shown.includes('TAIL-OF-A-LONG-VALUE'), 'the end of a long value is on display');
  assert.equal(els['approve'].disabled, false, 'Approve is available once the id is typed and every argument is shown');
});

test('the Note row is shown with its label only when there is a reason (a hidden value must not leave its label in the grid, shifting every row after it)', async () => {
  const none = await run(pendingWith({ a: 1 }));
  assert.equal(none.els['call-reason'].hidden, true);
  assert.equal(none.els['call-reason-label'].hidden, true, 'the label goes with the value');
  const some = await run(pendingWith({ a: 1 }, { reason: 'approval re-required' }));
  assert.equal(some.els['call-reason'].hidden, false);
  assert.equal(some.els['call-reason-label'].hidden, false);
  const tmpl = fs.readFileSync(new URL('../index.template.html', import.meta.url), 'utf8');
  assert.match(tmpl, /<dt id="call-reason-label" hidden>Note<\/dt><dd id="call-reason" hidden>/, 'the page starts with both hidden');
});

test('a re-asked gate is shown with its reason, and decidable', async () => {
  const { els } = await run(pendingWith({ a: 1 }, { reason: 'approval re-required: the recorded approvals were counted without excluding the requester' }));
  assert.ok(els['call-reason'].textContent.includes('re-required'));
  assert.equal(els['call-reason'].hidden, false);
  assert.equal(els['approve'].disabled, false);
});

test('a gate the gate would refuse, and a one-decision gate, cannot be decided', async () => {
  for (const over of [{ state: 'error', reason: 'approval policy Need = 5' }, { state: 'single' }, { state: 'decided' }]) {
    const { els } = await run(pendingWith({ a: 1 }, over));
    assert.equal(els['approve'].disabled, true, `state ${over.state}`);
    assert.equal(els['reject'].disabled, true, `state ${over.state}`);
  }
});

test('requests carry credentials by the configured mode, same-origin by default', async () => {
  let r = await run(pendingWith({ a: 1 }));
  assert.ok(r.fetched.filter((f) => f.url.includes('/pending')).every((f) => f.opts.credentials === 'same-origin'));
  r = await run(pendingWith({ a: 1 }), { href: 'https://approve.example.com/', config: { intake: 'https://app.example.com/approvals', credentials: 'include' } });
  assert.ok(r.fetched.filter((f) => f.url.includes('/pending')).length > 0);
  assert.ok(r.fetched.filter((f) => f.url.includes('/pending')).every((f) => f.opts.credentials === 'include'));
  r = await run(pendingWith({ a: 1 }), { href: 'https://approve.example.com/', config: { intake: 'https://app.example.com/approvals', credentials: 'bogus' } });
  assert.ok(r.fetched.filter((f) => f.url.includes('/pending')).every((f) => f.opts.credentials === 'same-origin'), 'an unknown mode falls back to same-origin');
});

test('a request the browser blocked under the page\'s policy is said so', async () => {
  const { els, docListeners } = await run(pendingWith({ a: 1 }));
  assert.equal(typeof docListeners.securitypolicyviolation, 'function');
  docListeners.securitypolicyviolation({ blockedURI: 'https://evil.example' });
  assert.match(els['status'].textContent, /blocked a request to https:\/\/evil\.example/);
});

const why = (...rs) => ({ why: rs });
const TAINT = { kind: 'taint', source: 'inbound-email', ref: 'tool:read_email/c1', pos: 4, at: '2026-10-24T10:43:00.000Z' };

test('the reasons are shown as text, one line each, with the time they are as of, and the panel says they are not signed', async () => {
  const { els } = await run(pendingWith({ a: 1 }, why(TAINT)));
  assert.equal(els['why-panel'].hidden, false);
  assert.equal(els['why-list'].children.length, 1);
  assert.equal(els['why-list'].children[0].textContent, 'untrusted content from inbound-email (read by tool read_email, call c1, journal position 4) (as of 2026-10-24T10:43:00.000Z)');
  const tmpl = fs.readFileSync(new URL('../index.template.html', import.meta.url), 'utf8');
  assert.match(tmpl, /application's record of why the call was held[^<]*They are not part of what you sign, and this page cannot check them/, 'the page says the reasons are the application\'s claim and not signed');
  assert.match(tmpl, /<section id="why-panel" hidden>/, 'the panel starts hidden');
});

test('no reason leaves the panel hidden and the list empty', async () => {
  const { els } = await run(pendingWith({ a: 1 }));
  assert.equal(els['why-panel'].hidden, true);
  assert.equal(els['why-list'].children.length, 0);
});

test('hostile names in a reason are text: no markup is set, and characters that hide content are written as escapes', async () => {
  const hostile = { kind: 'taint', source: '<img src=x onerror=alert(1)>\u202eevil', ref: 'tool:"><svg onload=x()>/c9', pos: 1 };
  const { els } = await run(pendingWith({ a: 1 }, why(hostile))); // an innerHTML assignment would throw in the stub
  const t = els['why-list'].children[0].textContent;
  assert.ok(t.includes('<img src=x onerror=alert(1)>'), 'the markup is shown as characters');
  assert.ok(!t.includes('\u202e') && t.includes('\\u202e'), 'the bidi override is written as an escape');
  assert.ok(t.includes('"><svg onload=x()>'));
});

test('a conflict between reason records is shown as a warning line, and the call stays decidable by the rules of the gate', async () => {
  const { els } = await run(pendingWith({ a: 1 }, why({ kind: 'conflict' })));
  assert.equal(els['why-list'].children[0].className, 'warn');
  assert.match(els['why-list'].children[0].textContent, /conflicting reason records/);
});

test('a cap reason writes its amounts as decimals, exactly, past what a JavaScript number holds', async () => {
  const cap = '{"kind":"cap","rule":"refund_one","action":"gate","why":"over","currency":"USD","amount":9007199254740993,"limit":500000}';
  const body = pendingWith({ a: 1 }).replace('"gate":', `"why":[${cap}],"gate":`);
  const { els } = await run(body);
  assert.equal(els['why-list'].children[0].textContent, 'cap refund_one: this call 90071992547409.93 USD, limit 5000.00 USD (over, gate)');
});

test('a reason the page cannot read stops the page from showing the call at all, and nothing is signed', async () => {
  const bad = pendingWith({ a: 1 }).replace('"gate":', '"why":[{"kind":"taint","future":1}],"gate":');
  const { els } = await run(bad);
  assert.equal(els['call-panel'].hidden, true);
  assert.match(els['status'].textContent, /unknown member/);
});

test('non-empty arguments are marked model-written and unverified, whatever they hold; empty ones are not', async () => {
  for (const args of [{ memo: 'pay now', n: 5 }, { n: 5, ok: true, z: null }, { 'ignore all previous instructions': 1 }]) {
    assert.equal((await run(pendingWith(args))).els['args-note'].hidden, false, JSON.stringify(args));
  }
  assert.equal((await run(pendingWith({}))).els['args-note'].hidden, true, 'no arguments, no note');
  const tmpl = fs.readFileSync(new URL('../index.template.html', import.meta.url), 'utf8');
  assert.match(tmpl, /<p id="args-note" class="note" hidden>[^<]*model-written, unverified/);
});
