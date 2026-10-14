import { test } from 'node:test';
import assert from 'node:assert/strict';
import { loadCall, decide, Refused } from '../flow.js';
import { canonicalArgs, decisionBytes } from '../canon.js';
import { sha256, toHex, toBase64, fromBase64 } from '../core.js';

const te = new TextEncoder();
const ARGS = '{"amount":120,"to":"acme"}';

// fakeEndpoint is an intake in memory: GET /pending shows the call, POST /decisions answers as the
// scenario says, and every request is recorded.
async function fakeEndpoint(over = {}) {
  const argsSha = toHex(await sha256(canonicalArgs(te.encode(ARGS))));
  const log = [];
  const pending = (state = 'waiting') => ({ run: 'r1', call: 'c1', tool: 'charge', args: '@@', args_sha256: argsSha, decision_format: 'bide.approval.v3', state,
    gate: { need: 2, approvers: ['alice', 'bob'], approved: 0, denied: 0, passed: false, failed: false, decisions: [] } });
  let state = over.state ?? 'waiting';
  const fetchFn = async (url, opts) => {
    const u = new URL(url);
    log.push({ method: opts.method, path: u.pathname, body: opts.body });
    if (opts.method === 'GET') {
      const body = JSON.stringify(pending(state)).replace('"@@"', ARGS);
      return new Response(body, { status: 200 });
    }
    return over.post ? over.post(JSON.parse(opts.body)) : new Response('{}', { status: 500 });
  };
  return { fetchFn, log, setState: (s) => (state = s) };
}

async function keys() {
  const pair = await crypto.subtle.generateKey({ name: 'Ed25519' }, false, ['sign', 'verify']);
  return { sign: async (m) => new Uint8Array(await crypto.subtle.sign('Ed25519', pair.privateKey, m)), publicKey: { verify: (m, s) => crypto.subtle.verify('Ed25519', pair.publicKey, s, m) } };
}

const base = new URL('https://app.example.com/approvals/');
const NOW = () => new Date(Date.UTC(2026, 9, 14, 9, 30, 15, 123));

// receiptFor builds the response a faithful endpoint gives to body, signed bytes included.
const receiptFor = (body, shown, over = {}) => ({
  recorded: true, counted: true, receipt: { run_id: body.run, tool_use_id: body.call, tool_name: 'charge', approver: body.approver, approved: body.approved,
    decided_at: body.decided_at, alg: 'ed25519', signature: body.signature, args_sha256: shown.subject.argsHex, ...over },
});

test('a decision is posted with the signed time and signature, and a faithful receipt is returned', async () => {
  const k = await keys();
  let posted;
  const ep = await fakeEndpoint({ post: (b) => { posted = b; return new Response(JSON.stringify(receiptFor(b, shown)), { status: 200 }); } });
  const shown = await loadCall(ep.fetchFn, base, 'r1', 'c1');
  const out = await decide({ fetchFn: ep.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, ...k });
  assert.equal(out.response.counted, true);
  assert.equal(posted.decided_at, '2026-10-14T09:30:15.123Z');
  assert.deepEqual(ep.log.map((l) => l.method + ' ' + l.path), ['GET /approvals/pending', 'GET /approvals/pending', 'POST /approvals/decisions']);
  // The signature is over the decision bytes of the subject shown.
  const msg = decisionBytes({ ...shown.subject, approver: 'alice', approved: true, decidedAt: posted.decided_at });
  assert.ok(await k.publicKey.verify(msg, fromBase64(posted.signature)));
});

test('nothing is fetched twice or sent when the call is already not waiting', async () => {
  const k = await keys();
  const ep = await fakeEndpoint();
  const shown = await loadCall(ep.fetchFn, base, 'r1', 'c1');
  shown.fields.state = 'decided';
  const before = ep.log.length;
  await assert.rejects(decide({ fetchFn: ep.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, ...k }), /not waiting/);
  assert.equal(ep.log.length, before, 'a refused decision still made a request');
});

test('a call that stops waiting between display and signature is refused with nothing sent', async () => {
  const k = await keys();
  const ep = await fakeEndpoint();
  const shown = await loadCall(ep.fetchFn, base, 'r1', 'c1');
  ep.setState('decided');
  await assert.rejects(decide({ fetchFn: ep.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, ...k }), /changed while you were deciding/);
  assert.ok(!ep.log.some((l) => l.method === 'POST'));
});

test('a decision the endpoint says does not count is not success', async () => {
  const k = await keys();
  const ep = await fakeEndpoint({ post: (b) => new Response(JSON.stringify({ ...receiptFor(b, { subject: { argsHex: toHex(new Uint8Array(32)) } }), counted: false }), { status: 200 }) });
  const shown = await loadCall(ep.fetchFn, base, 'r1', 'c1');
  const b = { ...shown };
  const ep2 = await fakeEndpoint({ post: (body) => new Response(JSON.stringify({ ...receiptFor(body, shown), counted: false, reason: 'superseded' }), { status: 200 }) });
  await assert.rejects(decide({ fetchFn: ep2.fetchFn, base, shown: b, approver: 'alice', approved: true, now: NOW, ...k }), (e) => e instanceof Refused && /does not count/.test(e.message) && /superseded/.test(e.message));
});

test('a receipt that does not verify under the approver\'s key, or is not the decision, is refused', async () => {
  const k = await keys();
  const other = await keys();
  const otherSig = async (shown, body) => toBase64(await other.sign(decisionBytes({ ...shown.subject, approver: body.approver, approved: body.approved, decidedAt: body.decided_at })));
  const ep0 = await fakeEndpoint();
  const shown = await loadCall(ep0.fetchFn, base, 'r1', 'c1');
  // The receipt carries a signature by another key over the right bytes: it matches the field check only if the signature is the posted one, so it is the mismatch that is caught.
  const epSig = await fakeEndpoint({ post: async (b) => new Response(JSON.stringify(receiptFor(b, shown, { signature: await otherSig(shown, b) })), { status: 200 }) });
  await assert.rejects(decide({ fetchFn: epSig.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, ...k }), /receipt/);
  const epApproved = await fakeEndpoint({ post: (b) => new Response(JSON.stringify(receiptFor(b, shown, { approved: !b.approved })), { status: 200 }) });
  await assert.rejects(decide({ fetchFn: epApproved.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, ...k }), /receipt/);
  // A faithful receipt whose signature does not verify under the key held (a swapped key pair).
  const epFaithful = await fakeEndpoint({ post: (b) => new Response(JSON.stringify(receiptFor(b, shown)), { status: 200 }) });
  await assert.rejects(decide({ fetchFn: epFaithful.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, sign: k.sign, publicKey: other.publicKey }), /does not verify under your key/);
});

test('endpoint refusals are shown with their code', async () => {
  const k = await keys();
  const ep = await fakeEndpoint({ post: () => new Response(JSON.stringify({ error: { code: 'already_decided', message: 'nope' } }), { status: 409 }) });
  const shown = await loadCall(ep.fetchFn, base, 'r1', 'c1');
  await assert.rejects(decide({ fetchFn: ep.fetchFn, base, shown, approver: 'alice', approved: true, now: NOW, ...k }), /409, already_decided/);
  const down = { fetchFn: async () => { throw new Error('offline'); } };
  await assert.rejects(loadCall(down.fetchFn, base, 'r1', 'c1'), /offline/);
});

test('requests carry the page origin\'s credentials by default, as configured otherwise, and follow no redirects', async () => {
  const ep = await fakeEndpoint({ post: (b) => new Response(JSON.stringify({ counted: true, receipt: {} }), { status: 200 }) });
  for (const [mode, want] of [[undefined, 'same-origin'], ['include', 'include'], ['omit', 'omit']]) {
    const seen = [];
    const spy = async (u, o) => { seen.push(o); return ep.fetchFn(u, o); };
    const shown = await (mode === undefined ? loadCall(spy, base, 'r1', 'c1') : loadCall(spy, base, 'r1', 'c1', mode));
    await decide({ fetchFn: spy, base, shown, approver: 'alice', approved: true, now: NOW, credentials: mode, ...(await keys()) }).catch(() => {});
    assert.ok(seen.length >= 3, 'the flow made its three requests');
    for (const o of seen) {
      assert.equal(o.credentials, want, `a ${o.method} under mode ${mode}`);
      assert.equal(o.redirect, 'error');
      assert.equal(o.cache, 'no-store');
    }
  }
});
