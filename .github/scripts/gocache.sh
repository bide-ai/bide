#!/usr/bin/env bash
# gocache.sh: prepare the restored Go build and test cache for a test job.
#
#   gocache.sh prepare   FRESH=true clears the cached test results; so does a change of the module
#                        files (go.mod, go.sum, go.work, go.work.sum) or of the Go version since the
#                        cache was saved. The build cache stays: it is content-addressed.
#
# CI restores the cache the last push to main saved (.github/actions/go-cache) and runs the tests
# without -count=1, so a package whose test binary and inputs are unchanged reuses its result. Two
# kinds of test result are not safe to reuse, and are handled here and in shards.sh:
#   - tests that read go.mod, go.sum or go.work through a subprocess (go list, go mod edit): go
#     test does not see those files change, so a change to any module file clears all results;
#   - tests that build or run another package (go build): listed in .github/uncached-tests.txt and
#     always run (check-uncached.sh keeps the list complete).
# Soundness for tests that read files of the tree relies on go test's record of the files a test
# opens (content hashes, and the listing of a directory it reads) and on every CI job starting from a
# fresh checkout, whose file modification times are all new: nothing here depends on a stale mtime
# matching. Files outside the module root are not recorded at all.
# A push to main runs every test fresh (FRESH=true) and saves the cache, so a stale result cannot
# outlive the next merge.
set -euo pipefail

[ "${1:-}" = prepare ] || { echo "usage: $0 prepare" >&2; exit 2; }

cache=$(go env GOCACHE)
marker=$cache/bide-modsum
mkdir -p "$cache"
sum=$( { go env GOVERSION; git ls-files -z -- '*go.mod' '*go.sum' 'go.work' 'go.work.sum' | xargs -0 cat; } | git hash-object --stdin)
if [ "${FRESH:-}" = true ]; then
  echo "fresh run: clearing cached test results"
  go clean -testcache
elif [ "$(cat "$marker" 2>/dev/null || true)" != "$sum" ]; then
  echo "module files or Go version changed since the cache was saved: clearing cached test results"
  go clean -testcache
else
  echo "keeping cached test results (module files unchanged)"
fi
echo "$sum" > "$marker"
