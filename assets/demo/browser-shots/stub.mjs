// A local stand-in for Slack's incoming webhook and PagerDuty's Events API: it records each request body.
import http from 'node:http';
import fs from 'node:fs';
const dir = process.argv[2];
let n = 0;
http.createServer((req, res) => {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    if (req.method !== 'POST') { res.writeHead(200); res.end('stub'); return; }
    const slack = req.url.startsWith('/slack');
    const name = `${String(++n).padStart(2, '0')}-${slack ? 'slack' : 'pagerduty'}`;
    fs.writeFileSync(`${dir}/${name}.json`, Buffer.concat(chunks));
    fs.writeFileSync(`${dir}/${name}.headers`, JSON.stringify({ method: req.method, url: req.url, ...req.headers }, null, 1));
    if (slack) { res.writeHead(200, { 'content-type': 'text/plain' }); res.end('ok'); }
    else { res.writeHead(202, { 'content-type': 'application/json' }); res.end('{"status":"success","message":"Event processed","dedup_key":"stub"}'); }
  });
}).listen(8641, '127.0.0.1', () => console.log('stub up'));
