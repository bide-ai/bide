// e2e.mjs: the page's decision flow (flow.js, over core.js and canon.js) against a real intake, run
// by the Go tests. The environment names the endpoint and the approver's key; the mode picks a
// scenario. It prints one JSON line: {"ok":true,...} or {"ok":false,"error":...}.
import { loadCall, decide, Refused } from '../flow.js';

const env = process.env;
const hex = (h) => new Uint8Array(Buffer.from(h, 'hex'));
const pkcs8 = Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(env.SEED_HEX, 'hex')]);
const priv = await crypto.subtle.importKey('pkcs8', pkcs8, 'Ed25519', false, ['sign']);
const pub = await crypto.subtle.importKey('raw', hex(env.PUB_HEX), 'Ed25519', true, ['verify']);

const base = new URL(env.BASE.endsWith('/') ? env.BASE : env.BASE + '/');
const run = env.RUN;
const call = env.CALL;
const approver = env.APPROVER;
const approved = env.APPROVED !== 'false';

try {
  const shown = await loadCall(fetch, base, run, call);
  const out = await decide({
    fetchFn: fetch,
    base,
    shown,
    approver,
    approved,
    sign: async (m) => new Uint8Array(await crypto.subtle.sign('Ed25519', priv, m)),
    publicKey: { verify: (m, s) => crypto.subtle.verify('Ed25519', pub, s, m) },
    now: () => new Date(env.NOW ?? Date.now()),
  });
  console.log(JSON.stringify({ ok: true, counted: out.response.counted, recorded: out.response.recorded, tool: shown.subject.tool, digest: shown.subject.argsHex, record: out.receipt.record }));
} catch (e) {
  console.log(JSON.stringify({ ok: false, refused: e instanceof Refused, error: e.message }));
}
