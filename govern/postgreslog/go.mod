module github.com/bide-ai/bide/govern/postgreslog

go 1.27.0

require (
	github.com/bide-ai/bide v0.0.0
	github.com/bide-ai/bide/govern v0.0.0
	github.com/jackc/pgx/v5 v5.11.0
)

require (
	github.com/blackwell-systems/gsm v0.13.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/bide-ai/bide => ../../

replace github.com/bide-ai/bide/govern => ../
