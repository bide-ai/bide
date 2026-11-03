// Copy the single-source docs from ../docs (and ../CHANGELOG.md) into this self-contained VitePress
// project at build time. The content is gitignored here (it lives in /docs);
// this keeps node module resolution inside docs-site while authoring stays in
// one place. Runs before docs:dev and docs:build.
import { cpSync, existsSync, mkdirSync } from 'node:fs'

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
