// The govern module: the Tier-2 governed-state layer over gsm. It is its own module so the
// core module (github.com/bide-ai/bide) does not depend on gsm; it stays v0.x until gsm is
// stable. It depends on the core only through its exported API, never through internal/.
module github.com/bide-ai/bide/govern

go 1.27.0

require (
	github.com/bide-ai/bide v0.11.0
	github.com/blackwell-systems/gsm v0.12.0
)

require (
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
