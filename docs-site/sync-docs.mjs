// Copy the single-source docs from ../docs (and ../CHANGELOG.md) into this self-contained VitePress
// project at build time. The content is gitignored here (it lives in /docs);
// this keeps node module resolution inside docs-site while authoring stays in
// one place. Runs before docs:dev and docs:build.
import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { rewriteLinks } from './rewrite-links.mjs'

const items = [
  'getting-started.md',
  'CONCEPTS.md',
  'ARCHITECTURE.md',
  'GUARANTEE.md',
  'KNOWN-LIMITATIONS.md',
  'tour.md',
  'ROADMAP.md',
  'formal-verification.md',
  'guides',
  'recipes',
  'reference',
  'testing',
  'trust',
  'blog',
]

for (const item of items) {
  const src = new URL(`../docs/${item}`, import.meta.url)
  const dest = new URL(`./${item}`, import.meta.url)
  if (existsSync(src)) {
    cpSync(src, dest, { recursive: true })
    console.log(`synced ${item}`)
  }
}

// The changelog lives at the repository root, not under docs/.
cpSync(new URL('../CHANGELOG.md', import.meta.url), new URL('./CHANGELOG.md', import.meta.url))
console.log('synced CHANGELOG.md')

// Links to repo files the site does not publish (source, examples, spec/, docs/design/**) become
// absolute GitHub URLs in the synced copies only. See rewrite-links.mjs.
const siteDir = fileURLToPath(new URL('./', import.meta.url))
const repoDir = path.resolve(siteDir, '..')
// Map a repo path to where the site has it: docs/X is synced to docs-site/X; CHANGELOG.md as is.
const siteCopy = (p) =>
  p === 'CHANGELOG.md' || (p.startsWith('docs/') && items.includes(p.split('/')[1]))
    ? path.join(siteDir, p.replace(/^docs\//, ''))
    : null // docs/README.md, docs/design/** and the like are not synced
const classify = (p) => {
  const copy = siteCopy(p)
  if (copy && existsSync(copy)) {
    const st = statSync(copy)
    if (st.isFile()) return 'page'
    if (existsSync(path.join(copy, 'index.md'))) return 'page'
  }
  const real = path.join(repoDir, p)
  return !existsSync(real) ? 'missing' : statSync(real).isDirectory() ? 'dir' : 'file'
}
const markdownIn = (dir) =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? markdownIn(path.join(dir, e.name)) : e.name.endsWith('.md') ? [path.join(dir, e.name)] : [],
  )
const synced = [...items.filter((i) => i.endsWith('.md')).map((i) => path.join(siteDir, i)), path.join(siteDir, 'CHANGELOG.md')]
for (const item of items) {
  const d = path.join(siteDir, item)
  if (existsSync(d) && statSync(d).isDirectory()) synced.push(...markdownIn(d))
}
let rewritten = 0
for (const file of synced) {
  if (!existsSync(file)) continue
  const rel = path.relative(siteDir, file).split(path.sep).join('/')
  const repoPath = rel === 'CHANGELOG.md' ? rel : `docs/${rel}`
  const before = readFileSync(file, 'utf8')
  const after = rewriteLinks(before, repoPath, classify)
  if (after !== before) {
    writeFileSync(file, after)
    rewritten++
  }
}
console.log(`rewrote repo links to GitHub URLs in ${rewritten} files`)

// Images the homepage shows. Copied from their single source (assets/ and docs/guides/img/) into
// public/home/ so no binary is duplicated in git. Add a file here when home-data.ts uses a new one.
const homeImages = [
  ['assets/hero.gif', 'hero.gif'],
  ['docs/guides/img/approve-page-waiting-dark.png', 'approve-page-waiting-dark.png'],
  ['docs/guides/img/run-viewer-config-risk-dark.png', 'run-viewer-config-risk-dark.png'],
  ['docs/guides/img/run-viewer-config-assert-dark.png', 'run-viewer-config-assert-dark.png'],
  ['docs/guides/img/flow-diff-weakened-dark.png', 'flow-diff-weakened-dark.png'],
  ['docs/guides/img/cli-approve-dark.gif', 'cli-approve-dark.gif'],
  ['docs/guides/img/cli-audit-notifications-pass-dark.png', 'cli-audit-notifications-pass-dark.png'],
  ['docs/guides/img/cli-audit-notifications-fail-dark.png', 'cli-audit-notifications-fail-dark.png'],
  ['docs/guides/img/flow-edit-structure.png', 'flow-edit-structure.png'],
  ['docs/guides/img/run-viewer-needs-attention-dark.png', 'run-viewer-needs-attention-dark.png'],
  ['docs/guides/img/run-viewer-taint-hold-dark.png', 'run-viewer-taint-hold-dark.png'],
  ['docs/guides/img/run-viewer-cap-refused-dark.png', 'run-viewer-cap-refused-dark.png'],
  ['docs/guides/img/run-viewer-stop-banner-dark.png', 'run-viewer-stop-banner-dark.png'],
]
mkdirSync(new URL('./public/home/', import.meta.url), { recursive: true })
for (const [from, to] of homeImages) {
  // A missing image fails the build loudly instead of shipping a broken homepage.
  cpSync(new URL(`../${from}`, import.meta.url), new URL(`./public/home/${to}`, import.meta.url))
}
console.log(`synced ${homeImages.length} homepage images`)
