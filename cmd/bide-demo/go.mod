// The bide-demo command is its own module: it keeps its journal in SQLite, so it requires the
// store/sqlite module, whose driver the root module does not carry. Run it with
// go run github.com/bide-ai/bide/cmd/bide-demo@latest.
module github.com/bide-ai/bide/cmd/bide-demo

go 1.27.0

require (
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/store/sqlite v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.59.0 // indirect
)

replace github.com/bide-ai/bide => ../../

replace github.com/bide-ai/bide/store/sqlite => ../../store/sqlite
