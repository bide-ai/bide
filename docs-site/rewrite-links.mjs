// Pure link rewriting for the synced docs. The docs are written to be read on GitHub, so they link
// to source files, examples, spec/ and docs/design/** with repo-relative paths. The site does not
// publish those targets, so on bide-ai.com each such link is rewritten to an absolute GitHub URL.
// The docs source is never modified; only the synced copy under docs-site/ is.
import path from 'node:path/posix'

export const REPO_URL = 'https://github.com/bide-ai/bide'

const IMAGE_EXT = /\.(png|jpe?g|gif|svg|webp|ico)$/i

/**
 * Rewrite repo-relative links in one Markdown file.
 *
 * @param {string} text         file contents
 * @param {string} repoPath     the file's path in the repo, e.g. "docs/guides/x.md" or "CHANGELOG.md"
 * @param {(p: string) => 'page' | 'file' | 'dir' | 'missing'} classify
 *   what a normalized repo path is: 'page' (the site serves it, or it is a synced asset), or, for
 *   anything else, whether it exists in the repo as a file or directory.
 * @returns {string}
 */
export function rewriteLinks(text, repoPath, classify) {
  const dir = path.dirname(repoPath)

  // Returns the replacement for one link target, or the target unchanged.
  const fix = (target) => {
    // Leave anchors, absolute site paths, and anything with a scheme (http:, mailto:, ...).
    if (!target || target.startsWith('#') || target.startsWith('/') || /^[a-z][a-z0-9+.-]*:/i.test(target)) return target
    const m = /^([^#?]*)([?#].*)?$/.exec(target)
    const [, rawPath, suffix = ''] = m
    if (!rawPath) return target
    let decoded = rawPath
    try { decoded = decodeURI(rawPath) } catch { /* keep as written */ }
    const resolved = path.normalize(path.join(dir, decoded)).replace(/\/$/, '')
    if (resolved.startsWith('..')) return target // escapes the repo; leave it for the build to flag
    const kind = classify(resolved)
    if (kind === 'missing') return target
    if (kind === 'page') {
      // The site serves CHANGELOG.md at its root while the repo has it above docs/, so relative
      // links to or from it must be recomputed. Every other page keeps its repo-relative layout.
      if (repoPath !== 'CHANGELOG.md' && resolved !== 'CHANGELOG.md') return target
      const siteOf = (p) => (p.startsWith('docs/') ? p.slice(5) : p)
      const rel = path.relative(path.dirname(siteOf(repoPath)), siteOf(resolved))
      return (rel.startsWith('.') ? rel : rel === '' ? '.' : rel) + suffix
    }
    const base = kind === 'dir' ? 'tree/main' : IMAGE_EXT.test(resolved) ? 'raw/main' : 'blob/main'
    return `${REPO_URL}/${base}/${encodeURI(resolved)}${suffix}`
  }

  const inline = /(\]\()(<[^>]*>|[^)\s]*)((?:\s+"[^"]*")?\))/g
  const refDef = /^(\s{0,3}\[[^\]]+\]:\s*)(\S+)/
  const htmlAttr = /(\b(?:href|src)=")([^"]*)(")/g
  const codeSpan = /(`+)(?:(?!\1)[\s\S])+?\1/g

  // Rewrite links in a line, skipping any whose target lies inside an inline code span. The link
  // text itself may contain code, as in [`pkg`](../pkg).
  const rewriteLine = (line) => {
    const spans = [...line.matchAll(codeSpan)].map((m) => [m.index, m.index + m[0].length])
    const inCode = (i) => spans.some(([a, b]) => i >= a && i < b)
    return line
      .replace(inline, (m, a, t, c, off) => (inCode(off + a.length) ? m : t.startsWith('<') ? a + '<' + fix(t.slice(1, -1)) + '>' + c : a + fix(t) + c))
      .replace(htmlAttr, (m, a, t, c, off) => (inCode(off) ? m : a + fix(t) + c))
  }

  let fence = null // the opening fence marker while inside a fenced code block
  return text
    .split('\n')
    .map((line) => {
      const f = /^\s{0,3}(`{3,}|~{3,})/.exec(line)
      if (fence) {
        if (f && f[1][0] === fence[0] && f[1].length >= fence.length) fence = null
        return line
      }
      if (f) {
        fence = f[1]
        return line
      }
      const def = refDef.exec(line)
      if (def) return def[1] + fix(def[2]) + line.slice(def[0].length)
      return rewriteLine(line)
    })
    .join('\n')
}
