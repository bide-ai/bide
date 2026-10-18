Section: Fixed

The release's `go mod tidy` made `github.com/jackc/pgx/v5` a direct requirement of `cmd/bide` and failed `TestDirectRequires`, because a journalcmd test imported its `stdlib` package directly while `go.mod` listed it as indirect; the test now relies on the driver `store/postgres` registers, and the stale indirect requirements in four example modules are tidied. CI now runs `go mod tidy` in every module and fails if it changes a `go.mod` or `go.sum`, so a direct import of a module marked indirect fails on the pull request and not at release time.
