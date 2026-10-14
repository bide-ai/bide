// flow.js: the whole of a decision, as the page performs it, over an injected fetch and key so the
// tests run it against a real endpoint under Node: fetch the call and show it (the caller's
// `show`), then, on the approver's confirmation, fetch it again, refuse on any difference, sign the
// subject captured at display time, post it, and check the receipt that comes back.

import { decisionBytes, formatTimestamp } from './canon.js';
import { checkPending, changedSince, parsePending, preflight, receiptProblem, toBase64, fromBase64 } from './core.js';

export class Refused extends Error {}

// credentials is fetch's credentials mode: 'same-origin' by default, so the cookies, HTTP
// authentication and TLS client certificate of the page's own origin reach the endpoint the way
// they do for a page the endpoint serves; 'include' for an endpoint on another origin that
// authenticates with a cookie or a client certificate (the deployment says so in config.json, and
// the intake must allow credentials: approvals.WithCORSCredentials); 'omit' for none.
// loadCall fetches GET /pending and returns {fields, args, subject}, throwing Refused for a call the
// endpoint describes inconsistently or that is not the one asked for.
export async function loadCall(fetchFn, base, run, call, credentials = 'same-origin') {
  const url = new URL('pending', base);
  url.search = new URLSearchParams({ run, call }).toString();
  const resp = await fetchFn(url.href, { method: 'GET', redirect: 'error', cache: 'no-store', credentials, headers: { Accept: 'application/json' } });
  const bytes = new Uint8Array(await resp.arrayBuffer());
  if (!resp.ok) throw new Refused(await describeError(resp.status, bytes));
  let parsed;
  try {
    parsed = parsePending(bytes);
  } catch (e) {
    throw new Refused(e.message);
  }
  try {
    const subject = await checkPending(parsed, run, call);
    return { fields: parsed.fields, args: parsed.args, subject };
  } catch (e) {
    throw new Refused(e.message);
  }
}

async function describeError(status, bytes) {
  try {
    const e = JSON.parse(new TextDecoder().decode(bytes)).error;
    if (e && e.code) return `the endpoint refused (HTTP ${status}, ${e.code}): ${e.message}`;
  } catch {
    // not an error body
  }
  return `the endpoint answered HTTP ${status}, which is not a pending call or a decision`;
}

// decide signs and posts the approver's decision on the call shown (shown = loadCall's result).
// key is {privateKey, publicRaw}; sign is (bytes) => Promise<Uint8Array> by that key. It returns
// {response, receipt} on success, and throws Refused with nothing signed or sent on any difference
// between what was shown and what the endpoint says now.
export async function decide({ fetchFn, base, shown, approver, approved, sign, publicKey, credentials = 'same-origin', now = () => new Date() }) {
  const { run, call } = shown.subject;
  const first = preflight(shown.fields, approver);
  if (first) throw new Refused(first);
  const again = await loadCall(fetchFn, base, run, call, credentials).catch((e) => {
    throw new Refused(`the endpoint changed while you were deciding; nothing was signed: ${e.message}`);
  });
  const why = preflight(again.fields, approver);
  if (why) throw new Refused(`the endpoint changed while you were deciding; nothing was signed: ${why}`);
  const diff = changedSince(shown.subject, shown.fields, again.subject, again.fields);
  if (diff) throw new Refused(`the call or its gate changed while you were deciding (${diff}); nothing was signed`);

  // What is signed is the subject captured at display time, never one rebuilt from the second fetch.
  const decidedAt = formatTimestamp(now());
  const msg = decisionBytes({ ...shown.subject, approver, approved, decidedAt });
  const signature = await sign(msg);
  const signatureB64 = toBase64(signature);
  const body = JSON.stringify({ run, call, approver, approved, decided_at: decidedAt, alg: 'ed25519', signature: signatureB64 });
  let resp;
  try {
    resp = await fetchFn(new URL('decisions', base).href, { method: 'POST', redirect: 'error', cache: 'no-store', credentials, headers: { 'Content-Type': 'application/json', Accept: 'application/json' }, body });
  } catch (e) {
    throw new Refused(`the request failed, and the decision may or may not have been recorded: reload this page to see the gate (a decision of yours already recorded is refused as already decided): ${e.message}`);
  }
  const bytes = new Uint8Array(await resp.arrayBuffer());
  if (!resp.ok) throw new Refused(await describeError(resp.status, bytes));
  let out;
  try {
    out = JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    throw new Refused('the decision was sent, and the endpoint\'s answer is not a decision response (it may have been recorded)');
  }
  const d = { ...shown.subject, approver, approved, decidedAt, signatureB64 };
  const problem = receiptProblem(out.receipt, d);
  if (problem) throw new Refused(`the decision was recorded, but the receipt the endpoint returned is refused (${problem}), so none is offered`);
  // The receipt must verify under the approver's own key.
  if (!(await publicKey.verify(msg, fromBase64(out.receipt.signature)))) throw new Refused('the decision was recorded, but the receipt does not verify under your key, so none is offered');
  if (out.counted !== true) throw new Refused(`the decision was recorded, but it does not count: another decision of yours counts (${out.reason ?? 'an earlier decision of yours'})`);
  return { response: out, receipt: out.receipt };
}
