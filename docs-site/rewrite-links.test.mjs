// Run with: node --test rewrite-links.test.mjs
import test from 'node:test'
import assert from 'node:assert/strict'
import { rewriteLinks } from './rewrite-links.mjs'

const pages = new Set(['docs/guides/a.md', 'docs/guides/img/x.png', 'docs/CONCEPTS.md'])
const dirs = new Set(['agent', 'docs/design', 'examples'])
const files = new Set(['agent/run.go', 'CONTRIBUTING.md', 'docs/design/d.md', 'assets/hero.gif'])
const classify = (p) => (pages.has(p) ? 'page' : dirs.has(p) ? 'dir' : files.has(p) ? 'file' : 'missing')
const rw = (t) => rewriteLinks(t, 'docs/guides/g.md', classify)
const G = 'https://github.com/bide-ai/bide'

test('site pages and assets are left alone', () => {
  const t = '[a](a.md#x) ![i](img/x.png) [c](../CONCEPTS.md)'
  assert.equal(rw(t), t)
})
test('files and dirs go to GitHub, anchors kept', () => {
  assert.equal(rw('[r](../../agent/run.go#L3)'), `[r](${G}/blob/main/agent/run.go#L3)`)
  assert.equal(rw('[d](../../examples/)'), `[d](${G}/tree/main/examples)`)
  assert.equal(rw('[d](../design/d.md "t")'), `[d](${G}/blob/main/docs/design/d.md "t")`)
})
test('images outside the site use raw', () => {
  assert.equal(rw('![h](../../assets/hero.gif)'), `![h](${G}/raw/main/assets/hero.gif)`)
})
test('reference defs and html attrs', () => {
  assert.equal(rw('[c]: ../../CONTRIBUTING.md'), `[c]: ${G}/blob/main/CONTRIBUTING.md`)
  assert.equal(rw('<a href="../../agent/run.go">x</a>'), `<a href="${G}/blob/main/agent/run.go">x</a>`)
})
test('code in link text, and link text split across lines (target only on this line)', () => {
  assert.equal(rw('[`a`](../../agent/run.go)'), `[\`a\`](${G}/blob/main/agent/run.go)`)
  assert.equal(rw('wrapped](../../agent/run.go)'), `wrapped](${G}/blob/main/agent/run.go)`)
})
test('pages move when CHANGELOG.md is the source or the target', () => {
  assert.equal(rewriteLinks('[g](docs/guides/a.md#s)', 'CHANGELOG.md', classify), '[g](guides/a.md#s)')
  assert.equal(rewriteLinks('[c](../CHANGELOG.md)', 'docs/ROADMAP.md', (p) => (p === 'CHANGELOG.md' ? 'page' : 'missing')), '[c](CHANGELOG.md)')
})
test('urls, anchors, missing targets and code are untouched', () => {
  for (const t of ['[x](https://e.com)', '[x](#a)', '[x](../../nope.go)', '`[x](../../agent/run.go)`', '```\n[x](../../agent/run.go)\n```']) {
    assert.equal(rw(t), t)
  }
  assert.equal(rw('`c` [x](../../agent/run.go)'), `\`c\` [x](${G}/blob/main/agent/run.go)`)
})
