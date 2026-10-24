// The reasons the Chrome test has the page draw (chrome/why-fixture.mjs) are read by the page's own code, and
// the lines it expects are the lines that code writes: so the Chrome test's list check is a check of the page.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parsePending, reasonLine, safeText } from '../core.js';
import { EXPECTED, WHY, injectWhy } from './chrome/why-fixture.mjs';

const te = new TextEncoder();

test('the Chrome rig\'s reasons are valid JSON, and the answer with them still reads', () => {
  JSON.parse(WHY);
  const body = '{"run":"r","call":"c1","tool":"t","args":{},"args_sha256":"00","decision_format":"bide.approval.v3","state":"waiting","gate":{"need":1,"approvers":["a"]}}';
  const got = parsePending(te.encode(injectWhy(body)));
  assert.equal(got.fields.why.length, 4);
  assert.equal(got.fields.tool, 't', 'the rest of the answer is as it was');
  assert.equal(new TextDecoder().decode(got.args), '{}');
});

test('the lines the Chrome test expects are the ones the page writes', () => {
  const body = '{"run":"r","call":"c1","tool":"t","args":{},"args_sha256":"00","decision_format":"bide.approval.v3","state":"waiting","gate":{"need":1,"approvers":["a"]}}';
  const why = parsePending(te.encode(injectWhy(body))).fields.why;
  // app.js writes each as safeText(reasonLine(r)) and " (as of <at>)" when the reason has a time.
  const lines = why.map((r) => safeText(reasonLine(r)) + (r.at ? ` (as of ${safeText(r.at)})` : ''));
  assert.deepEqual(lines, EXPECTED);
});
