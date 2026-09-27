module github.com/blackwell-systems/bide/govern/redislog

go 1.27.0

require (
	github.com/blackwell-systems/gsm v0.11.0
	github.com/blackwell-systems/bide v0.0.0
	github.com/redis/go-redis/v9 v9.22.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/blackwell-systems/bide => ../../
