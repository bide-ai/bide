// Self-contained example module: it imports store/sqlite (an on-disk agent.Durable that
// carries the modernc.org/sqlite dependency, which the root module does not), so, like the
// other example modules, it is its own module. Build and run it from this directory:
// GOWORK=off go run .
module github.com/blackwell-systems/bide/examples/plan

go 1.27.0

require (
	github.com/blackwell-systems/bide v0.0.0
	github.com/blackwell-systems/bide/store/sqlite v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.59.0 // indirect
)

replace github.com/blackwell-systems/bide => ../../

replace github.com/blackwell-systems/bide/store/sqlite => ../../store/sqlite
