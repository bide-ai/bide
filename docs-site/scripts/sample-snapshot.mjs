#!/usr/bin/env node
// Builds a static, read-only snapshot of `bide view` on the sample journal into
// docs-site/public/sample/, so the site can serve it at /sample/.
//
// It changes no bide code. It builds bide from this checkout, runs `bide sample` (which writes the
// sample journal and serves the viewer on loopback), also serves the sample's flow configs with
// `bide view -config` (and one `-diff`), crawls every reachable page and asset, rewrites the
// root-relative links to /sample/..., adds a small "static snapshot" banner and writes the files.
// It fails (exit 1) on any request that is not a 200, any internal link that does not resolve, and
// any machine-specific string in the output. The servers and temp directories it creates are
// removed on exit.
//
// Usage: node scripts/sample-snapshot.mjs      (needs Go, see go.mod; run from anywhere)
//        BIDE_BIN=/path/to/bide node ...       (skip the build and use this binary)

import { spawn, spawnSync } from 'node:child_process'
import { createInterface } from 'node:readline'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const repo = path.resolve(here, '..', '..')
const outDir = path.resolve(here, '..', 'public', 'sample')
const PREFIX = '/sample' // where the snapshot is served on the site

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'bide-sample-snapshot-'))
const children = []
function cleanup() {
  for (const c of children) {
    try { c.kill('SIGTERM') } catch {}
  }
  try { fs.rmSync(tmp, { recursive: true, force: true }) } catch {}
}
process.on('exit', cleanup)
for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => process.exit(130))

function fail(msg) {
  console.error('sample-snapshot: ' + msg)
  process.exit(1)
}

// ---- build bide and write the sample -------------------------------------------------------------

let bin = process.env.BIDE_BIN
if (!bin) {
  bin = path.join(tmp, 'bide')
  console.log('sample-snapshot: building bide')
  const r = spawnSync('go', ['build', '-o', bin, '.'], { cwd: path.join(repo, 'cmd', 'bide'), stdio: 'inherit' })
  if (r.status !== 0) fail('go build failed')
}

// start runs bide with args and resolves with the loopback origin it serves on, read from the line
// that carries "http://127.0.0.1:PORT/" (bide sample prints "viewer  http://...", bide view prints
// its address on stderr or stdout depending on the mode, so both are read).
function start(args, cwd) {
  return new Promise((resolve, reject) => {
    const c = spawn(bin, args, { cwd, stdio: ['ignore', 'pipe', 'pipe'] })
    children.push(c)
    let done = false
    const seen = []
    const onLine = (line) => {
      seen.push(line)
      const m = /http:\/\/(127\.0\.0\.1:\d+)/.exec(line)
      if (m && !done) {
        done = true
        resolve({ origin: 'http://' + m[1], child: c })
      }
    }
    createInterface({ input: c.stdout }).on('line', onLine)
    createInterface({ input: c.stderr }).on('line', onLine)
    c.on('exit', (code) => {
      if (!done) reject(new Error(`bide ${args[0]} exited ${code} before serving:\n${seen.join('\n')}`))
    })
    const timer = setTimeout(() => { if (!done) reject(new Error(`bide ${args[0]} did not report an address:\n${seen.join('\n')}`)) }, 30000)
    timer.unref()
  })
}

// ---- URL to file mapping ------------------------------------------------------------------------

const safeID = /^[A-Za-z0-9._-]+$/

// A page the viewer serves, as the site path of its snapshot file (no leading slash) or null for a
// URL that is not one. base is the site directory of this server's pages ('' for the sample's own).
function fileFor(u, base) {
  const url = new URL(u, 'http://x')
  const p = url.pathname
  const q = url.searchParams
  const id = q.get('id')
  const state = q.get('state')
  let rel
  if (p === '/' && [...q.keys()].length === 0) rel = 'index.html'
  else if (p === '/' && [...q.keys()].join() === 'state' && safeID.test(state)) rel = `state/${state}/index.html`
  else if (p === '/attention' && [...q.keys()].length === 0) rel = 'attention/index.html'
  else if (p === '/run' && [...q.keys()].join() === 'id' && safeID.test(id)) rel = `run/${id}/index.html`
  else if (p === '/run.json' && [...q.keys()].join() === 'id' && safeID.test(id)) rel = `run/${id}/run.json`
  else if (p === '/config.json' && [...q.keys()].length === 0) rel = 'config.json'
  else if (p === '/diff.json' && [...q.keys()].length === 0) rel = 'diff.json'
  else if (/^\/assets\/[A-Za-z0-9._-]+$/.test(p) && [...q.keys()].length === 0) return 'assets' + p.slice('/assets'.length) // shared by every server
  else return null
  return base ? `${base}/${rel}` : rel
}

// The URL the site serves a snapshot file at.
function publicURL(file) {
  const dir = file.endsWith('/index.html') ? file.slice(0, -'index.html'.length) : file === 'index.html' ? '' : file
  return `${PREFIX}/${dir}`
}

// ---- banner and page rewrites -------------------------------------------------------------------

const captured = new Date().toISOString().slice(0, 10)
const flows = [
  { dir: 'flows/refund-v1', label: 'refund flow', args: ['view', '-config', 'flows/refund-v1.yaml'] },
  { dir: 'flows/refund-v2', label: 'weakened gate (diff)', args: ['view', '-config', 'flows/refund-v2.yaml', '-diff', 'flows/refund-v1.yaml'] },
  { dir: 'flows/refund-bypass', label: 'bypass', args: ['view', '-config', 'flows/refund-bypass.yaml'] },
]

const banner =
  `<div class="snapshot-banner" role="note"><span><strong>Static snapshot of sample data</strong> (captured ${captured}): ` +
  `read-only, no live updates, nothing runs. ` +
  `<a href="${PREFIX}/">Runs</a> · <a href="${PREFIX}/attention/">Needs attention</a> · ` +
  flows.map((f) => `<a href="${PREFIX}/${f.dir}/">${f.label}</a>`).join(' · ') +
  `</span><span><a href="https://bide-ai.com/">bide-ai.com</a> · ` +
  `run it yourself: <code>bide sample</code></span></div>`

const bannerCSS = `.snapshot-banner{display:flex;flex-wrap:wrap;gap:.25rem 1rem;justify-content:space-between;padding:.5rem 1rem;font-size:.8125rem;line-height:1.4;background:var(--card-2,#f3f4f6);color:var(--fg,#111);border-bottom:1px solid var(--line,#d0d5dd)}
.snapshot-banner a{color:var(--accent,#2563eb)}
.snapshot-banner code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.95em}
`

// rewriteLinks maps every root-relative href/src/action in html to its snapshot URL, queueing the
// pages it finds. home is where a link to "/" goes (the sample's run list for a flow server).
function rewriteLinks(html, base, queue, from) {
  return html.replace(/\b(href|src|action)="(\/[^"]*)"/g, (all, attr, val) => {
    if (val.startsWith('//')) return all
    const target = val.replace(/&amp;/g, '&')
    // A flow server's "/" is its own page, but the link back to "/" in its header means the run list.
    const isHome = base !== '' && target === '/'
    const file = isHome ? 'index.html' : fileFor(target, base)
    if (file === null) fail(`${from}: ${attr}="${val}" is not a page or asset the snapshot knows; the viewer changed, update scripts/sample-snapshot.mjs`)
    if (!isHome) queue(target, base, file)
    return `${attr}="${publicURL(file)}"`
  })
}

function rewritePage(html, base, queue, from) {
  let out = html
  // The page reloads itself every few seconds (Needs attention); a snapshot does not change.
  out = out.replace(/<meta http-equiv="refresh"[^>]*>/g, '')
  out = out.replace('; this page reloads every 5 seconds.', '.')
  // Live mode needs the server: drop its script, and the "live" pill that claims the page follows the run.
  out = out.replace(/<script src="\/assets\/live\.js"[^>]*><\/script>/g, '')
  out = out.replace(/<span class="pill live" id="live"[^>]*>live<\/span>/g, '')
  // The runs page's filter form (run ID, kind) is a GET form the server answers; the state links
  // beside it are plain pages and stay.
  out = out.replace(/<form class="filters"[\s\S]*?<\/form>/g, '<p class="muted small">Filtering by run ID and kind needs <code>bide view</code>; the state buttons above work here.</p>')
  out = rewriteLinks(out, base, queue, from)
  out = out.replace('</head>', `<link rel="stylesheet" href="${PREFIX}/assets/snapshot.css"></head>`)
  out = out.replace(/(<body[^>]*>)/, `$1${banner}`)
  if (!out.includes('snapshot-banner')) fail(`${from}: no <body> to put the banner in`)
  return out
}

// ---- crawl ---------------------------------------------------------------------------------------

const written = new Map() // file -> bytes
async function crawl(origin, base, seeds) {
  const seen = new Set()
  const pending = []
  const queue = (target, b, file) => {
    if (b !== base) return
    if (!seen.has(target)) { seen.add(target); pending.push({ target, file }) }
  }
  for (const s of seeds) queue(s, base, fileFor(s, base))
  while (pending.length) {
    const { target, file } = pending.shift()
    const res = await fetch(origin + target)
    if (res.status !== 200) fail(`GET ${target} answered ${res.status}`)
    const type = res.headers.get('content-type') || ''
    let body
    if (type.startsWith('text/html')) {
      body = Buffer.from(rewritePage(await res.text(), base, queue, target))
    } else if (file === 'assets/share.js') {
      let js = await res.text()
      // The share button shows only on /run; the snapshot's run pages live under /sample/run/.
      const patched = js.replace('location.pathname !== "/run"', `!location.pathname.startsWith("${PREFIX}/run/")`)
      if (patched === js) console.warn('sample-snapshot: warning: share.js no longer has the /run check; the share button may not show')
      body = Buffer.from(patched)
    } else if (file.endsWith('.js') || file.endsWith('.css') || file.endsWith('.json')) {
      body = Buffer.from(await res.arrayBuffer())
    } else {
      body = Buffer.from(await res.arrayBuffer())
    }
    const prev = written.get(file)
    if (prev && !prev.equals(body)) fail(`${file} came out differently from two servers`)
    written.set(file, body)
  }
}

// ---- main ----------------------------------------------------------------------------------------

const sampleDir = path.join(tmp, 'sample')
fs.mkdirSync(sampleDir)

const sample = await start(['sample', '-out', sampleDir, '-no-browser', '-addr', '127.0.0.1:0'], tmp).catch((e) => fail(e.message))
await crawl(sample.origin, '', ['/', '/attention'])
// Every run page is reached from the run list; the state filters too.

for (const f of flows) {
  const srv = await start([...f.args, '-addr', '127.0.0.1:0'], sampleDir).catch((e) => fail(e.message))
  await crawl(srv.origin, f.dir, ['/'])
  srv.child.kill('SIGTERM')
}
sample.child.kill('SIGTERM')

written.set('assets/snapshot.css', Buffer.from(bannerCSS))

// ---- checks --------------------------------------------------------------------------------------

// Every internal link in the output must resolve to a file written.
const files = new Set(written.keys())
const urlToFile = new Map([...files].map((f) => [publicURL(f), f]))
for (const [file, buf] of written) {
  if (!file.endsWith('.html')) continue
  for (const m of buf.toString().matchAll(/\b(?:href|src|action)="(\/sample[^"#]*)"/g)) {
    if (!urlToFile.has(m[1])) fail(`${file}: link ${m[1]} has no file in the snapshot`)
  }
}
// Machine-specific strings must not appear anywhere.
const needles = new Set([tmp, fs.realpathSync(tmp), os.homedir(), os.hostname(), os.userInfo().username, repo, 'localhost', '127.0.0.1'])
needles.delete('')
for (const [file, buf] of written) {
  if (/\.(png|jpe?g|gif|ico|woff2?)$/.test(file)) continue
  const text = buf.toString()
  for (const n of needles) {
    if (n.length >= 3 && text.includes(n)) fail(`${file} contains the machine-specific string ${JSON.stringify(n)}`)
  }
}
const htmlCount = [...files].filter((f) => f.endsWith('.html')).length
if (htmlCount < 20) fail(`only ${htmlCount} pages captured; expected the run list, 14 runs, Needs attention and the flow pages`)

// ---- write ---------------------------------------------------------------------------------------

fs.rmSync(outDir, { recursive: true, force: true })
for (const [file, buf] of written) {
  const dest = path.join(outDir, file)
  fs.mkdirSync(path.dirname(dest), { recursive: true })
  fs.writeFileSync(dest, buf)
}
console.log(`sample-snapshot: wrote ${written.size} files (${htmlCount} pages) to ${path.relative(process.cwd(), outDir) || '.'}`)
