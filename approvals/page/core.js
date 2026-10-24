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

// EXPONENTS is the ISO 4217 exponent table of internal/amount (the number of minor-unit digits of each
// currency): a Go test holds the two equal. It writes a cap reason's amount as a decimal; a currency not
// in it is shown in minor units, said so.
export const EXPONENTS = {
  AED: 2, AFN: 2, ALL: 2, AMD: 2, AOA: 2, ARS: 2, AUD: 2, AWG: 2, AZN: 2, BAM: 2, BBD: 2, BDT: 2, BHD: 3, BIF: 0,
  BMD: 2, BND: 2, BOB: 2, BOV: 2, BRL: 2, BSD: 2, BTN: 2, BWP: 2, BYN: 2, BZD: 2, CAD: 2, CDF: 2, CHE: 2, CHF: 2,
  CHW: 2, CLF: 4, CLP: 0, CNY: 2, COP: 2, COU: 2, CRC: 2, CUP: 2, CVE: 2, CZK: 2, DJF: 0, DKK: 2, DOP: 2, DZD: 2,
  EGP: 2, ERN: 2, ETB: 2, EUR: 2, FJD: 2, FKP: 2, GBP: 2, GEL: 2, GHS: 2, GIP: 2, GMD: 2, GNF: 0, GTQ: 2, GYD: 2,
  HKD: 2, HNL: 2, HTG: 2, HUF: 2, IDR: 2, ILS: 2, INR: 2, IQD: 3, IRR: 2, ISK: 0, JMD: 2, JOD: 3, JPY: 0, KES: 2,
  KGS: 2, KHR: 2, KMF: 0, KPW: 2, KRW: 0, KWD: 3, KYD: 2, KZT: 2, LAK: 2, LBP: 2, LKR: 2, LRD: 2, LSL: 2, LYD: 3,
  MAD: 2, MDL: 2, MGA: 2, MKD: 2, MMK: 2, MNT: 2, MOP: 2, MRU: 2, MUR: 2, MVR: 2, MWK: 2, MXN: 2, MXV: 2, MYR: 2,
  MZN: 2, NAD: 2, NGN: 2, NIO: 2, NOK: 2, NPR: 2, NZD: 2, OMR: 3, PAB: 2, PEN: 2, PGK: 2, PHP: 2, PKR: 2, PLN: 2,
  PYG: 0, QAR: 2, RON: 2, RSD: 2, RUB: 2, RWF: 0, SAR: 2, SBD: 2, SCR: 2, SDG: 2, SEK: 2, SGD: 2, SHP: 2, SLE: 2,
  SOS: 2, SRD: 2, SSP: 2, STN: 2, SVC: 2, SYP: 2, SZL: 2, THB: 2, TJS: 2, TMT: 2, TND: 3, TOP: 2, TRY: 2, TTD: 2,
  TWD: 2, TZS: 2, UAH: 2, UGX: 0, USD: 2, USN: 2, UYI: 0, UYU: 2, UYW: 4, UZS: 2, VED: 2, VES: 2, VND: 0, VUV: 0,
  WST: 2, XAD: 2, XAF: 0, XCD: 2, XCG: 2, XOF: 0, XPF: 0, YER: 2, ZAR: 2, ZMW: 2, ZWG: 2,
};

const WHY_TEXT_KEYS = ['kind', 'source', 'ref', 'rule', 'action', 'why', 'currency', 'at'];
const WHY_KEYS = new Set([...WHY_TEXT_KEYS, 'pos', 'amount', 'limit']);
const MAX_WHY = 64;
const MAX_WHY_TEXT = 512;

// parseWhy reads the reasons the endpoint gives for a gated call (approvals.Pending.Why: agent.GateReason, an
// array of objects) strictly: bounded, with no unknown member, text members as strings and the numbers as
// integers, kept as the digits written (an amount can be past what a JavaScript number holds). It returns
// plain objects whose pos is a number and whose amount and limit are digit strings, and throws an Error
// naming the problem. The reasons are the application's claim: nothing here is a check of them.
export function parseWhy(raw) {
  const r = parseFirst(raw);
  if (r === null || !r.value.a) throw new Error('the endpoint\'s reasons are not a JSON array');
  if (r.value.a.length > MAX_WHY) throw new Error(`the endpoint gives more than ${MAX_WHY} reasons`);
  return r.value.a.map((v) => {
    if (v === null || !v.o) throw new Error('a reason is not a JSON object');
    const out = {};
    for (const [k, x] of v.o) {
      if (!WHY_KEYS.has(k)) throw new Error(`a reason has an unknown member ${safeText(JSON.stringify(k))}`);
      if (WHY_TEXT_KEYS.includes(k)) {
        if (typeof x !== 'string' || x.length > MAX_WHY_TEXT) throw new Error(`a reason's ${k} is not a short string`);
        out[k] = x;
      } else {
        if (x === null || x.num === undefined || !/^-?[0-9]+$/.test(x.num)) throw new Error(`a reason's ${k} is not an integer`);
        out[k] = k === 'pos' ? Number(x.num) : x.num;
      }
    }
    if (typeof out.kind !== 'string' || out.kind === '') throw new Error('a reason has no kind');
    return out;
  });
}

// money writes minor units (a digit string) in a currency as the decimal text it is shown in: "5000.00 USD".
// A currency the table does not hold is written in minor units, and says so.
export function money(minor, currency) {
  let n = BigInt(minor);
  const sign = n < 0n ? '-' : '';
  if (n < 0n) n = -n;
  const exp = Object.prototype.hasOwnProperty.call(EXPONENTS, currency) ? EXPONENTS[currency] : null;
  if (exp === null) return `${sign}${n} minor units of ${JSON.stringify(currency)}`;
  if (exp === 0) return `${sign}${n} ${currency}`;
  const p = 10n ** BigInt(exp);
  return `${sign}${n / p}.${String(n % p).padStart(exp, '0')} ${currency}`;
}

// reasonLine is a reason as one line of text, the same line Go's agent.GateReason.Line writes (a test holds
// the two to one golden file). Every string in it is the application's, and the caller shows it with
// safeText and as text, never as markup.
export function reasonLine(r) {
  switch (r.kind) {
    case 'taint': {
      let where = r.ref ?? '';
      if (where.startsWith('tool:')) {
        const rest = where.slice(5);
        const i = rest.indexOf('/');
        where = `read by tool ${i < 0 ? rest : rest.slice(0, i)}, call ${i < 0 ? '' : rest.slice(i + 1)}`;
      } else if (where === 'input') where = "the run's input";
      else if (where === 'retrieval') where = 'retrieved documents';
      return `untrusted content from ${r.source ?? ''} (${where}, journal position ${r.pos ?? 0})`;
    }
    case 'cap': {
      const amt = r.amount !== undefined ? 'this call ' + money(r.amount, r.currency ?? '') : 'an amount that could not be read';
      return `cap ${r.rule ?? ''}: ${amt}, limit ${money(r.limit ?? '0', r.currency ?? '')} (${r.why ?? ''}, ${r.action ?? ''})`;
    }
    case 'conflict':
      return 'conflicting reason records for this call: none is shown, and the journal should be looked at';
  }
  return r.kind;
}

// PENDING_KEYS are the members of approvals.Pending (its json tags): a Go test fails when they drift.
const PENDING_KEYS = new Set(['run', 'call', 'tool', 'args', 'args_text', 'args_sha256', 'decision_format', 'state', 'asked_at', 'expires_at', 'reason', 'why', 'gate']);

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
    else if (name === 'why') fields.why = parseWhy(bytes.subarray(s, e));
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
  // A certificate-bound approver signs with a certificate chain, which this page cannot hold: the endpoint
  // would refuse a bare signature, so say where to decide instead of letting it be sent.
  if (g.identity && Object.prototype.hasOwnProperty.call(g.identity, approver)) return `${safeText(approver)} is a certificate-bound approver of this gate: decide with bide approve -to <this endpoint> -cert <your chain>, which signs with your certificate; this page signs with a bare key only`;
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

// argsHaveText reports whether the arguments hold text the model wrote: a string value anywhere in them, or
// arguments that are not JSON (shown as text). Numbers, booleans and null are not text.
export function argsHaveText(raw) {
  const t = goTrimSpace(raw);
  if (t.length === 0) return false;
  const r = parseFirst(t);
  if (r === null) return true;
  const walk = (v) => {
    if (typeof v === 'string') return true;
    if (v === null || typeof v === 'boolean' || v.num !== undefined) return false;
    if (v.a) return v.a.some(walk);
    return [...v.o.values()].some(walk);
  };
  return walk(r.value);
}
