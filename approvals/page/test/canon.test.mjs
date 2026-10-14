import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { canonicalArgs, decisionBytes, formatTimestamp, goTrimSpace } from '../canon.js';

const vectors = JSON.parse(readFileSync(new URL('../../../agent/testdata/canonical_args_vectors.json', import.meta.url), 'utf8'));
const b64 = (s) => new Uint8Array(Buffer.from(s, 'base64'));
const hex = (u) => Buffer.from(u).toString('hex');

test('the canonical arguments match Go on every vector', () => {
  assert.ok(vectors.args.length > 300, 'the vector file is missing cases');
  let bad = 0;
  for (const v of vectors.args) {
    const got = canonicalArgs(b64(v.input_b64));
    const want = b64(v.canonical_b64);
    if (Buffer.compare(Buffer.from(got), Buffer.from(want)) !== 0) {
      bad++;
      if (bad < 6) console.error(`vector ${JSON.stringify(v.name)}:\n  got  ${JSON.stringify(Buffer.from(got).toString('latin1')).slice(0, 200)}\n  want ${JSON.stringify(Buffer.from(want).toString('latin1')).slice(0, 200)}`);
    }
    assert.equal(createHash('sha256').update(want).digest('hex'), v.sha256, v.name);
  }
  assert.equal(bad, 0, `${bad} vectors differ from Go`);
});

test('the decision bytes match Go on every vector', () => {
  for (const v of vectors.decisions) {
    const args = b64(v.args_b64);
    const digest = createHash('sha256').update(canonicalArgs(args)).digest();
    const got = decisionBytes({ run: v.run, call: v.call, tool: v.tool, argsDigest: digest, approver: v.approver, approved: v.approved, decidedAt: v.decided_at });
    assert.equal(hex(got), v.bytes_hex, v.name);
  }
});

test('key order is by UTF-8 bytes, not UTF-16 code units', () => {
  const got = Buffer.from(canonicalArgs(new TextEncoder().encode('{"\\ud800\\udc00":1,"\\ue000":2}'))).toString('utf8');
  assert.equal(got, '{"":2,"\u{10000}":1}');
});

test('the trim is Go\'s, over UTF-8', () => {
  assert.equal(Buffer.from(goTrimSpace(new TextEncoder().encode('  x 　'))).toString(), 'x');
  assert.equal(goTrimSpace(new Uint8Array([0xff, 0x20])).length, 1); // an invalid byte stops the trim
});

test('timestamps are the one canonical form', () => {
  assert.equal(formatTimestamp(new Date(Date.UTC(2026, 9, 14, 9, 30, 15, 123))), '2026-10-14T09:30:15.123Z');
  assert.equal(formatTimestamp(new Date(Date.UTC(2026, 9, 14, 9, 30, 15, 0))), '2026-10-14T09:30:15.000Z');
  assert.throws(() => formatTimestamp(new Date(NaN)));
});

import { parseFirst } from '../canon.js';

test('top-level member offsets are the raw bytes of each value', () => {
  const text = '{"a": [1, 2.50 ,{"x":1}], "args":{"b":1.0,"a":"\\u00e9"} ,"n":1e3,"a":true}';
  const b = new TextEncoder().encode(text);
  const r = parseFirst(b, true);
  const raw = (name) => r.members.filter((m) => m[0] === name).map((m) => text.slice(m[1], m[2]));
  assert.deepEqual(raw('args'), ['{"b":1.0,"a":"\\u00e9"}']);
  assert.deepEqual(raw('a'), ['[1, 2.50 ,{"x":1}]', 'true']); // duplicates are reported, in order
  assert.deepEqual(raw('n'), ['1e3']);
});
