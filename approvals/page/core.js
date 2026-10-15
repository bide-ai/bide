// core.js: what the approve page decides, apart from the browser: reading the pending call the
// endpoint shows, checking it, and displaying text so that it cannot hide content. The browser
// glue (app.js) calls these, and the tests call them under Node.

import { canonicalArgs, goTrimSpace, parseFirst, quote, serialize, utf8Decode } from './canon.js';

export const DECISION_FORMAT = 'bide.approval.v3';

const dec = new TextDecoder();
const enc = new TextEncoder();

// unsafeRune is approveline.Unsafe: a character that could change how text is shown (a control, a
// format character such as a bidirectional override or a zero-width joiner, a line or paragraph
// separator, a space other than the ASCII one, a combining mark, or a filler that renders blank).
const FIXED = new Set([0x115f, 0x1160, 0x3164, 0xffa0, 0x2800, 0x180e, 0x2028, 0x2029, 0xfffc]);
const CATEGORIES = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Zs}\p{Mn}\p{Me}]/u;
function unsafeRune(cp) {
  if (FIXED.has(cp)) return true;
  if (cp === 0x20) return false;
  return CATEGORIES.test(String.fromCodePoint(cp));
}

// safeText returns s with every character that could change how it is shown written as an escape
// (\uXXXX, or \UXXXXXXXX), so the reader sees that something is there. A newline is kept.
export function safeText(s) {
  let out = '';
  for (const ch of s) {
    const cp = ch.codePointAt(0);
    if (cp === 0x0a) out += ch;
    else if (unsafeRune(cp)) out += cp > 0xffff ? '\\U' + cp.toString(16).padStart(8, '0') : '\\u' + cp.toString(16).padStart(4, '0');
    else out += ch;
  }
  return out;
}

// decodeLossless reads bytes as UTF-8 with an escape (\xNN) for each invalid byte, so the display of
// non-text arguments shows what is there.
export function decodeLossless(b) {
  let out = '';
  for (let i = 0; i < b.length; ) {
    const [r, w] = utf8Decode(b, i);
    if (r === 0xfffd && w === 1) out += '\\x' + b[i].toString(16).padStart(2, '0');
    else out += String.fromCodePoint(r);
    i += w;
  }
  return out;
}

const INDENT = '  ';

// prettyArgs shows the arguments as indented JSON with keys sorted and numbers as written, or, for
// bytes that are not one JSON value, as the quoted text the signature binds. The result is safe to
// show (safeText).
export function prettyArgs(raw) {
  const trimmed = goTrimSpace(raw);
  if (trimmed.length === 0) return '{}';
  const r = parseFirst(trimmed);
  let ok = r !== null;
  if (ok) {
    let k = r.end;
    while (k < trimmed.length && [0x20, 0x09, 0x0a, 0x0d].includes(trimmed[k])) k++;
    ok = !(k < trimmed.length && trimmed[k] !== 0x5d && trimmed[k] !== 0x7d);
  }
  if (!ok) return safeText('not JSON: ' + quote(decodeLossless(trimmed)));
  return safeText(pretty(r.value, 0));
}

function pretty(v, depth) {
  if (v === null) return 'null';
  if (typeof v === 'boolean') return String(v);
  if (typeof v === 'string') return quote(v);
  if (v.num !== undefined) return v.num;
  if (depth >= 100) return dec.decode(serialize(v)); // deeper than a reader follows: compact, still all shown
  const pad = INDENT.repeat(depth + 1);
  if (v.a) {
    if (v.a.length === 0) return '[]';
    return '[\n' + v.a.map((x) => pad + pretty(x, depth + 1)).join(',\n') + '\n' + INDENT.repeat(depth) + ']';
  }
  if (v.o.size === 0) return '{}';
  const keys = [...v.o.keys()].sort((x, y) => cmpCodePoints(x, y));
  return '{\n' + keys.map((k) => pad + quote(k) + ': ' + pretty(v.o.get(k), depth + 1)).join(',\n') + '\n' + INDENT.repeat(depth) + '}';
}

function cmpCodePoints(x, y) {
  const a = Array.from(x, (c) => c.codePointAt(0));
  const b = Array.from(y, (c) => c.codePointAt(0));
  for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
  return a.length - b.length;
}

export async function sha256(bytes) {
  return new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
}

export const toHex = (u) => Array.from(u, (x) => x.toString(16).padStart(2, '0')).join('');
export const fromHex = (h) => new Uint8Array(h.match(/../g).map((x) => parseInt(x, 16)));
export const toBase64 = (u) => btoa(String.fromCharCode(...u));
export const fromBase64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

// PENDING_KEYS are the members of approvals.Pending (its json tags): a Go test fails when they drift.
const PENDING_KEYS = new Set(['run', 'call', 'tool', 'args', 'args_text', 'args_sha256', 'decision_format', 'state', 'asked_at', 'expires_at', 'reason', 'gate']);

// parsePending reads the endpoint's answer to GET /pending strictly: exactly one JSON object, with
// no repeated and no unknown member, and the arguments taken as the raw bytes the endpoint sent
// (their number spellings and escapes intact, never re-serialized). It returns the fields and the
// arguments' bytes, and throws an Error naming the problem.
export function parsePending(bytes) {
  const r = parseFirst(bytes, true);
  if (r === null || !r.value.o) throw new Error('the endpoint did not answer with a JSON object');
  let k = r.end;
  while (k < bytes.length && [0x20, 0x09, 0x0a, 0x0d].includes(bytes[k])) k++;
  if (k !== bytes.length) throw new Error('the endpoint answered with more than one JSON value');
  const seen = new Set();
  const fields = {};
  let argsRaw = null;
  for (const [name, s, e] of r.members) {
    if (!PENDING_KEYS.has(name)) throw new Error(`the endpoint's answer has an unknown member ${safeText(JSON.stringify(name))}`);
    if (seen.has(name)) throw new Error(`the endpoint's answer repeats the member ${name}`);
    seen.add(name);
    if (name === 'args') argsRaw = bytes.subarray(s, e);
    else fields[name] = JSON.parse(dec.decode(bytes.subarray(s, e)));
  }
  if (argsRaw !== null && fields.args_text !== undefined) throw new Error('the endpoint sent the arguments both as JSON and as text');
  const args = argsRaw !== null ? argsRaw : enc.encode(fields.args_text ?? '');
  return { fields, args };
}

// checkPending verifies what the endpoint showed is the call asked for and is consistent: the same
// run and call, the decision format this page signs, and the arguments' digest computed here from
// the arguments shown equal to the endpoint's own. It returns the subject the page signs.
export async function checkPending(parsed, run, call) {
  const f = parsed.fields;
  if (f.run !== run || f.call !== call) throw new Error(`the endpoint described run ${safeText(String(f.run))} call ${safeText(String(f.call))}, not the run and call asked for; nothing was signed`);
  if (f.decision_format !== DECISION_FORMAT) throw new Error(`the endpoint's decision format is ${safeText(String(f.decision_format))}, and this page signs ${DECISION_FORMAT}; nothing was signed`);
  if (typeof f.tool !== 'string' || !f.gate || !Array.isArray(f.gate.approvers) || !Number.isInteger(f.gate.need)) throw new Error('the endpoint answered with an incomplete call');
  const digest = await sha256(canonicalArgs(parsed.args));
  if (toHex(digest) !== f.args_sha256) throw new Error(`the endpoint's arguments digest (${safeText(String(f.args_sha256))}) is not the digest of the arguments it shows (${toHex(digest)}): it is inconsistent, and nothing was signed`);
  return { run, call, tool: f.tool, argsDigest: digest, argsHex: toHex(digest) };
}

// preflight refuses a decision the endpoint's own report says cannot be made.
export function preflight(fields, approver) {
  if (fields.state === 'error') return `the gate cannot be evaluated, so this call takes no decision: ${safeText(String(fields.reason ?? 'the endpoint gives no reason'))}`;
  if (fields.state === 'single') return 'this is a single-approver gate, which takes no signed decision';
  if (fields.state === 'expired') return `the gate's deadline has passed${fields.expires_at ? ` (${safeText(String(fields.expires_at))})` : ''}: it takes no more decisions, and the call will not run`;
  if (fields.state !== 'waiting') return `the call is not waiting for a decision (the endpoint says it is ${safeText(String(fields.state))})`;
  const g = fields.gate;
  if (!g.approvers.includes(approver)) return `${safeText(approver)} is not an approver of this gate; its approvers are: ${safeText(g.approvers.join(', '))}`;
  for (const d of g.decisions ?? []) if (d.approver === approver && d.counted) return `${safeText(approver)} already decided on this call: ${d.approved ? 'approved' : 'denied'}`;
  if (g.passed) return 'the gate already has its approvals (as the endpoint counts them); nothing more is needed';
  if (g.failed) return 'the gate can no longer pass (as the endpoint counts them)';
  return null;
}

// changedSince reports how the call or its gate differs between the display and a later fetch, or
// null when what matters is the same: the run, call, tool, arguments digest, decision format, and
// who must approve.
export function changedSince(shown, shownFields, now, nowFields) {
  if (shown.run !== now.run || shown.call !== now.call) return 'the run or call';
  if (shown.tool !== now.tool) return 'the tool';
  if (shown.argsHex !== now.argsHex) return 'the arguments';
  if (shownFields.decision_format !== nowFields.decision_format) return 'the decision format';
  if (shownFields.gate.need !== nowFields.gate.need || shownFields.gate.approvers.join('\n') !== nowFields.gate.approvers.join('\n')) return 'who must approve';
  return null;
}

// keyFingerprint is the key identity (audit.KeyID) of an Ed25519 public key: "ed25519:" and the hex
// SHA-256 of the 32 raw bytes, and a grouped form for reading aloud.
export async function keyFingerprint(rawPub) {
  const h = toHex(await sha256(rawPub));
  return { id: 'ed25519:' + h, grouped: h.match(/.{1,4}/g).join(' ') };
}

export const publicKeyText = (rawPub) => 'ed25519:' + toHex(rawPub);

// receiptProblem checks the receipt the endpoint returned against the decision signed: the same
// call, approver, verdict, time, scheme, signature and arguments digest, and no anchoring proof
// (only the approver's own verify-receipts -update adds one). It returns a description of the
// first mismatch, or null.
export function receiptProblem(rc, d) {
  if (rc === null || typeof rc !== 'object') return 'no receipt';
  if (rc.anchor !== undefined && rc.anchor !== null) return 'it carries an anchoring proof';
  const want = { run_id: d.run, tool_use_id: d.call, tool_name: d.tool, approver: d.approver, approved: d.approved, decided_at: d.decidedAt, alg: 'ed25519', signature: d.signatureB64, args_sha256: d.argsHex };
  for (const [k, v] of Object.entries(want)) if (rc[k] !== v) return `its ${k} is not what was signed`;
  return null;
}
