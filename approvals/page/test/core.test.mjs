import { test } from 'node:test';
import assert from 'node:assert/strict';
import { safeText, prettyArgs, clip, parsePending, checkPending, preflight, changedSince, receiptProblem, toHex, sha256, MAX_ARG_LINES } from '../core.js';
import { canonicalArgs } from '../canon.js';

const te = new TextEncoder();
const pendingJSON = async (over = {}, argsText = '{"amount":120,"to":"acme"}') => {
  const argsSha = toHex(await sha256(canonicalArgs(te.encode(argsText))));
  const p = { run: 'r1', call: 'c1', tool: 'charge', args: '@@ARGS@@', args_sha256: argsSha, decision_format: 'bide.approval.v3', state: 'waiting',
    gate: { need: 2, approvers: ['alice', 'bob', 'carol'], approved: 0, denied: 0, passed: false, failed: false }, ...over };
  return te.encode(JSON.stringify(p).replace('"@@ARGS@@"', argsText));
};

test('text that could hide content is shown as escapes', () => {
  assert.equal(safeText('a\u202eb'), 'a\\u202eb');            // bidirectional override
  assert.equal(safeText('x\u200by'), 'x\\u200by');           // zero-width space
  assert.equal(safeText('\u2800\u3164'), '\\u2800\\u3164');   // blank fillers
  assert.equal(safeText('a\u00a0b'), 'a\\u00a0b');           // no-break space
  assert.equal(safeText('e\u0301'), 'e\\u0301');             // a combining mark
  assert.equal(safeText('\x1b[2J'), '\\u001b[2J');           // a terminal escape
  assert.equal(safeText('\u{1F600}\u{E0001}'), '\u{1F600}\\U000e0001');
  assert.equal(safeText('line\nbreak and space'), 'line\nbreak and space');
});

test('arguments are shown sorted, numbers as written, unsafe characters escaped', () => {
  const shown = prettyArgs(te.encode('{"b":1.0,"a":"x\u202ey","c":[1e3,{}]}'));
  assert.equal(shown, '{\n  "a": "x\\u202ey",\n  "b": 1.0,\n  "c": [\n    1e3,\n    {}\n  ]\n}');
  assert.equal(prettyArgs(te.encode('')), '{}');
  assert.match(prettyArgs(te.encode('free text')), /^not JSON: "free text"$/);
  assert.match(prettyArgs(new Uint8Array([0x61, 0xff])), /\\xff/);
});

test('long arguments are clipped and the clip says how much is hidden', () => {
  const many = Array.from({ length: MAX_ARG_LINES + 5 }, (_, i) => `line ${i}`).join('\n');
  const c = clip(many);
  assert.equal(c.text.split('\n').length, MAX_ARG_LINES);
  assert.ok(c.hidden > 0);
  const wide = clip('x'.repeat(500));
  assert.ok(wide.text.endsWith(' [...]') && wide.hidden === 300);
  assert.equal(clip('short').hidden, 0);
});

test('a consistent pending call is accepted and its arguments are the raw bytes sent', async () => {
  const body = await pendingJSON({}, '{"b":1.0,"a":2}');
  const parsed = parsePending(body);
  assert.equal(new TextDecoder().decode(parsed.args), '{"b":1.0,"a":2}');
  const subject = await checkPending(parsed, 'r1', 'c1');
  assert.equal(subject.tool, 'charge');
  assert.equal(subject.argsHex.length, 64);
});

test('an endpoint that is inconsistent or off-target is refused', async () => {
  const good = await pendingJSON();
  const text = new TextDecoder().decode(good);
  await assert.rejects(checkPending(parsePending(te.encode(text.replace('"amount":120', '"amount":12'))), 'r1', 'c1'), /inconsistent/);
  await assert.rejects(checkPending(parsePending(good), 'r2', 'c1'), /not the run and call asked for/);
  await assert.rejects(checkPending(parsePending(te.encode(text.replace('bide.approval.v3', 'bide.approval.v2'))), 'r1', 'c1'), /decision format/);
  assert.throws(() => parsePending(te.encode('{"run":"a","run":"b"}')), /repeats/);
  assert.throws(() => parsePending(te.encode('{"extra":1}')), /unknown member/);
  assert.throws(() => parsePending(te.encode('[1]')), /JSON object/);
  assert.throws(() => parsePending(te.encode('{} {}')), /more than one/);
  assert.throws(() => parsePending(te.encode('{"args":{},"args_text":"x"}')), /both as JSON and as text/);
});

test('preflight refuses what the endpoint says cannot be decided', async () => {
  const f = (over) => ({ state: 'waiting', gate: { approvers: ['alice', 'bob'], decisions: [], passed: false, failed: false }, ...over });
  assert.equal(preflight(f({}), 'alice'), null);
  assert.match(preflight(f({ state: 'decided' }), 'alice'), /not waiting/);
  assert.match(preflight(f({}), 'mallory'), /not an approver/);
  assert.match(preflight(f({ gate: { approvers: ['alice'], decisions: [{ approver: 'alice', counted: true, approved: true }] } }), 'alice'), /already decided/);
  assert.match(preflight(f({ gate: { approvers: ['alice'], passed: true } }), 'alice'), /already has its approvals/);
  assert.match(preflight(f({ gate: { approvers: ['alice'], failed: true } }), 'alice'), /can no longer pass/);
});

test('a change in the call or its gate between display and signature is named', () => {
  const base = { run: 'r', call: 'c', tool: 't', argsHex: 'aa' };
  const fields = { decision_format: 'bide.approval.v3', gate: { need: 2, approvers: ['a', 'b'] } };
  assert.equal(changedSince(base, fields, { ...base }, JSON.parse(JSON.stringify(fields))), null);
  assert.equal(changedSince(base, fields, { ...base, tool: 'u' }, fields), 'the tool');
  assert.equal(changedSince(base, fields, { ...base, argsHex: 'bb' }, fields), 'the arguments');
  assert.equal(changedSince(base, fields, base, { ...fields, gate: { need: 1, approvers: ['a', 'b'] } }), 'who must approve');
  assert.equal(changedSince(base, fields, base, { ...fields, gate: { need: 2, approvers: ['a', 'c'] } }), 'who must approve');
  assert.equal(changedSince(base, fields, base, { ...fields, decision_format: 'x' }), 'the decision format');
});

test('a receipt that is not the decision signed, or carries a proof, is refused', () => {
  const d = { run: 'r', call: 'c', tool: 't', approver: 'a', approved: true, decidedAt: '2026-10-14T09:30:15.123Z', signatureB64: 'c2ln', argsHex: 'ab' };
  const rc = { run_id: 'r', tool_use_id: 'c', tool_name: 't', approver: 'a', approved: true, decided_at: d.decidedAt, alg: 'ed25519', signature: 'c2ln', args_sha256: 'ab' };
  assert.equal(receiptProblem(rc, d), null);
  assert.match(receiptProblem({ ...rc, approved: false }, d), /approved/);
  assert.match(receiptProblem({ ...rc, signature: 'x' }, d), /signature/);
  assert.match(receiptProblem({ ...rc, anchor: {} }, d), /anchoring proof/);
  assert.match(receiptProblem(null, d), /no receipt/);
});
