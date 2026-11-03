// Build-time data for the homepage badge: the latest released version (the first "## [x.y.z]"
// heading in the changelog, which sits under "Unreleased") and the Go version from go.mod.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export interface VersionData {
  version: string
  go: string
  /** Number of released versions in the changelog (rc and Unreleased excluded). */
  releases: number
  /** Date of the oldest release, YYYY-MM-DD. */
  since: string
  /** Figures from docs/formal-verification.md. */
  models: string
  configs: string
  bugs: string
}

declare const data: VersionData
export { data }

const changelog = fileURLToPath(new URL('../../../CHANGELOG.md', import.meta.url))
const formal = fileURLToPath(new URL('../../../docs/formal-verification.md', import.meta.url))
const gomod = fileURLToPath(new URL('../../../go.mod', import.meta.url))

export default {
  // Rebuild the data when either file changes in dev.
  watch: [changelog, gomod, formal],
  load(): VersionData {
    const log = readFileSync(changelog, 'utf8')
    const v = /^## \[(\d+\.\d+\.\d+[^\]]*)\]/m.exec(log)
    if (!v) throw new Error('version.data.ts: no "## [x.y.z]" heading found in CHANGELOG.md')
    const mod = readFileSync(gomod, 'utf8')
    const g = /^go (\d+\.\d+)/m.exec(mod)
    if (!g) throw new Error('version.data.ts: no "go x.y" line found in go.mod')

    // Released versions: "## [x.y.z] - date" headings, rc builds excluded.
    const rel = [...log.matchAll(/^## \[(\d+\.\d+\.\d+)\] - (\d{4}-\d{2}-\d{2})/gm)]
    if (rel.length === 0) throw new Error('version.data.ts: no "## [x.y.z] - date" headings in CHANGELOG.md')
    const since = rel.map((m) => m[2]).sort()[0]

    // Bold figures in the formal-verification page. Fail loudly if the wording drifts.
    const fv = readFileSync(formal, 'utf8')
    const fig = (re: RegExp, what: string) => {
      const m = re.exec(fv)
      if (!m) throw new Error(`version.data.ts: "${what}" not found in docs/formal-verification.md`)
      return m[1]
    }
    return {
      version: v[1],
      go: g[1],
      releases: rel.length,
      since,
      models: fig(/\*\*(\d+) models\*\*/, '**N models**'),
      configs: fig(/\*\*(\d+) configurations\*\*/, '**N configurations**'),
      bugs: fig(/\*\*(\d+) bugs found\*\*/, '**N bugs found**'),
    }
  },
}
