//go:build race

package flowedit

// opStride is how many of a file's operations the corpus measurement skips between the ones it
// runs. The race detector makes the full measurement take many minutes, and CI runs every test
// under it, so the race build measures every eighth operation (the data races it looks for do not
// depend on which operations run); the full measurement runs without it in the Editor corpus job
// (.github/workflows/editor-chrome.yml) and in a plain go test.
const opStride = 8
