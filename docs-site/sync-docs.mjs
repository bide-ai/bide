// Copy the single-source docs from ../docs into this self-contained VitePress
// project at build time. The content is gitignored here (it lives in /docs);
// this keeps node module resolution inside docs-site while authoring stays in
// one place. Runs before docs:dev and docs:build.
import { cpSync, existsSync } from 'node:fs'

const items = [
  'getting-started.md',
  'CONCEPTS.md',
  'GUARANTEE.md',
  'KNOWN-LIMITATIONS.md',
  'guides',
  'reference',
  'testing',
]

for (const item of items) {
  const src = new URL(`../docs/${item}`, import.meta.url)
  const dest = new URL(`./${item}`, import.meta.url)
  if (existsSync(src)) {
    cpSync(src, dest, { recursive: true })
    console.log(`synced ${item}`)
  }
}
