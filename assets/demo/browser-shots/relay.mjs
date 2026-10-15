// relay.mjs CAPDIR RELAYLOG: renders a Slack payload the relay really sent (captured by a local stub) as a Block Kit
// style preview, and screenshots it in both color schemes. The preview is ours (neutral styling, no
// Slack branding); it is labelled as a render of the payload, not a screenshot of Slack.
import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const [capDir, relayLog] = process.argv.slice(2);
const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
// mrkdwn: *bold*, `code`, <!date^ts^fmt|fallback>, and the &amp; &lt; &gt; entities Slack decodes.
function mrkdwn(s) {
  let out = '';
  const re = /<!date\^\d+\^[^|>]*\|([^>]*)>|`([^`]*)`|\*([^*]+)\*/g;
  let last = 0, m;
  const plain = (t) => esc(t.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&')).replace(/\n/g, '<br>');
  while ((m = re.exec(s))) {
    out += plain(s.slice(last, m.index));
    if (m[1] !== undefined) out += `<span class="date">${esc(m[1])}</span>`;
    else if (m[2] !== undefined) out += `<code>${plain(m[2])}</code>`;
    else out += `<b>${plain(m[3])}</b>`;
    last = re.lastIndex;
  }
  return out + plain(s.slice(last));
}
function block(b) {
  switch (b.type) {
    case 'header': return `<h2>${esc(b.text.text)}</h2>`;
    case 'section': return `<div class="sec"><p>${mrkdwn(b.text.text)}</p>${b.fields ? `<div class="fields">${b.fields.map((f) => `<div>${mrkdwn(f.text)}</div>`).join('')}</div>` : ''}</div>`;
    case 'actions': return `<div class="actions">${b.elements.map((e) => `<a class="btn" title="${esc(e.url)}">${esc(e.text.text)}</a>`).join('')}</div>`;
    case 'context': return `<div class="ctx">${b.elements.map((e) => mrkdwn(e.text)).join(' ')}</div>`;
    default: throw new Error('unrendered block type ' + b.type);
  }
}
const page = (payload, name, logLine, ack) => `<!doctype html><meta charset=utf-8><style>
:root { --bg:#f4f5f7; --card:#fff; --fg:#1c1d21; --mute:#5d6068; --line:#d9dbe0; --accent:#2b5fd9; --codebg:#eef0f3; }
@media (prefers-color-scheme: dark) { :root { --bg:#17181b; --card:#222428; --fg:#ececee; --mute:#a0a3ab; --line:#3a3d44; --accent:#7ea3f5; --codebg:#2e3035; } }
body { margin:0; background:var(--bg); color:var(--fg); font:15px/1.45 system-ui,sans-serif; }
.wrap { width:640px; padding:20px; box-sizing:border-box; }
.label { font-size:12.5px; color:var(--mute); margin:0 0 10px; }
.msg { background:var(--card); border:1px solid var(--line); border-left:4px solid var(--accent); border-radius:6px; padding:14px 16px; }
h2 { font-size:18px; margin:0 0 8px; }
.sec p { margin:0 0 10px; }
.fields { display:grid; grid-template-columns:1fr 1fr; gap:10px 16px; margin-bottom:10px; }
.fields div { overflow-wrap:anywhere; }
code { background:var(--codebg); border:1px solid var(--line); border-radius:3px; padding:0 4px; font:13px ui-monospace,monospace; }
.btn { display:inline-block; border:1px solid var(--line); border-radius:5px; padding:5px 12px; font-weight:600; color:var(--fg); margin:2px 0 8px; }
.ctx { color:var(--mute); font-size:12.5px; }
.foot { margin:10px 0 0; font:12px/1.4 ui-monospace,monospace; color:var(--mute); overflow-wrap:anywhere; }
</style><div class="wrap" id="shot">
<p class="label">Rendered from the Slack payload that <code>bide notify relay</code> delivered to a local stub. This is a preview of the payload's blocks, not a screenshot of Slack.</p>
<div class="msg">${payload.blocks.map(block).join('')}</div>
<p class="foot">${esc(logLine)}<br>the receiver's acknowledgement: ${esc(ack)}</p></div>`;

const log = fs.readFileSync(relayLog, 'utf8').split('\n');
// The stub numbers what it records in arrival order: the Slack bodies are the approval, then its all-clear.
const slack = fs.readdirSync(capDir).filter((f) => f.endsWith('-slack.json')).sort();
if (slack.length !== 2) throw new Error(`expected 2 Slack payloads, found ${slack.length}`);
const jobs = [
  { file: slack[0], name: 'notify-relay-slack-approval', line: log.find((l) => l.includes('approval_pending, run')) },
  { file: slack[1], name: 'notify-relay-slack-resolved', line: log.find((l) => l.includes('resolved, run')) },
];
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_BIN, headless: true, args: ['--no-first-run'] });
try {
  for (const j of jobs) {
    const payload = JSON.parse(fs.readFileSync(`${capDir}/${j.file}`, 'utf8'));
    const html = page(payload, j.name, j.line.replace(/^\d\d:\d\d:\d\d /, ''), 'HTTP 200 ok (the stub answers as Slack does)');
    
    for (const theme of ['dark', 'light']) {
      const p = await browser.newPage();
      await p.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: theme }]);
      await p.setViewport({ width: 640, height: 600, deviceScaleFactor: 2 });
      await p.setContent(html);
      const el = await p.$('#shot');
      await el.screenshot({ path: `${process.env.OUT}/${j.name}-${theme}.png` });
      await p.close();
    }
  }
} finally { await browser.close(); }
