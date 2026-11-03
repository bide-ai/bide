// Build-time data for the homepage badge: the latest released version (the first "## [x.y.z]"
// heading in the changelog, which sits under "Unreleased") and the Go version from go.mod.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export interface VersionData {
  version: string
  go: string
}

declare const data: VersionData
export { data }

const changelog = fileURLToPath(new URL('../../../CHANGELOG.md', import.meta.url))
const gomod = fileURLToPath(new URL('../../../go.mod', import.meta.url))

export default {
  // Rebuild the data when either file changes in dev.
  watch: [changelog, gomod],
  load(): VersionData {
    const log = readFileSync(changelog, 'utf8')
    const v = /^## \[(\d+\.\d+\.\d+[^\]]*)\]/m.exec(log)
    if (!v) throw new Error('version.data.ts: no "## [x.y.z]" heading found in CHANGELOG.md')
    const mod = readFileSync(gomod, 'utf8')
    const g = /^go (\d+\.\d+)/m.exec(mod)
    if (!g) throw new Error('version.data.ts: no "go x.y" line found in go.mod')
    return { version: v[1], go: g[1] }
  },
}
