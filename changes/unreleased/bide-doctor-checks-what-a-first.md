Section: Added

`bide doctor`: checks what a first run depends on and prints a result with a fix hint for each: the Go version (1.27 or later), `bide` against the latest release (skipped offline), that SQLite works in a temporary directory, a Postgres DSN from `BIDE_POSTGRES_DSN` (or the variable `-postgres-env` names) if set, which model API key variables are set (names only, never values), that `GOPATH` and the module cache are writable, and in a `bide init` project that it builds. Colored on a terminal like the other commands (`-color`, `NO_COLOR`). Exit 1 only when a check failed.
