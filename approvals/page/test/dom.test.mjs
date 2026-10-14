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
  return { textContent: '', hidden: false, disabled: false, value: '', dataset: {}, className: '', href: '', scrollHeight: 0, clientHeight: 100, scrollTop: 0,
    addEventListener(t, f) { (listeners[t] ??= []).push(f); }, fire(t) { return Promise.all((listeners[t] ?? []).map((f) => f())); } };
}

// run loads the page against an endpoint that answers pendingBody, and returns the elements.
async function run(pendingBody, { href = 'https://app.example/approvals/page/', config = null } = {}) {
  const els = new Proxy({}, { get: (t, k) => (t[k] ??= el()) });
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
  const ctx = { document: { getElementById: (id) => els[id], addEventListener: (t, f) => { docListeners[t] = f; } }, indexedDB, crypto: globalThis.crypto, TextEncoder, TextDecoder, URL, URLSearchParams, Blob, btoa, atob, Uint8Array,
    location: { href, hash: '#run=r1&call=c1', origin: new URL(href).origin },
    fetch: async (u, o) => {
      fetched.push({ url: String(u), opts: o });
      if (String(u).endsWith('config.json')) return config ? { ok: true, json: async () => config } : { ok: false };
      return { ok: true, status: 200, arrayBuffer: async () => new TextEncoder().encode(pendingBody).buffer };
    },
    addEventListener() {}, console, Promise, Array, Map, Set, Math, Date, JSON, Error, Object, String, Number, Symbol, Function, setTimeout, queueMicrotask };
  vm.createContext(ctx);
  vm.runInContext(script, ctx);
  await new Promise((r) => setTimeout(r, 200));
  await els['create-key'].fire('click');
  els['approver'].value = 'ops';
  await els['approver'].fire('input');
  await new Promise((r) => setTimeout(r, 50));
  return { els, fetched, docListeners };
}

const pendingWith = (args, over = {}) => {
  const a = typeof args === 'string' ? args : JSON.stringify(args);
  const digest = createHash('sha256').update(canonicalArgs(new TextEncoder().encode(a))).digest('hex');
  const p = { run: 'r1', call: 'c1', tool: 'wire', args: '@@', args_sha256: digest, decision_format: 'bide.approval.v3', state: 'waiting',
    gate: { need: 2, approvers: ['ops', 'fin', 'risk'], approved: 0, denied: 0, pending: ['ops', 'fin', 'risk'], passed: false, failed: false }, ...over };
  return JSON.stringify(p).replace('"@@"', a);
};

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
