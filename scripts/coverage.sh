#!/usr/bin/env bash
# coverage.sh: run the tests of the published modules with coverage and merge the per-module
# profiles into one Go coverage profile for Codecov (.github/workflows/coverage.yml) or for a look
# at the numbers locally.
#
#   scripts/coverage.sh [OUT]   (default OUT: coverage.out in the current directory)
#   scripts/coverage.sh --list  (print the modules it measures)
#
# Each module is tested from its own directory with GOWORK=off, as CI does, with -covermode=atomic
# and -coverpkg=./..., so code in the module counts when any test in the module runs it (the
# conformance and test-helper packages are exercised by other packages' tests). Every package's run
# then repeats the blocks it covers, so the profiles are merged by internal/tools/covmerge, which
# keeps one line per block with the highest count. Files are named by import path, distinct across
# modules. The tests that need PG_DSN or REDIS_ADDR run when those are set in the environment (the
# Coverage workflow starts the services) and skip otherwise. The examples, the benchmarks and the
# integration module are not measured; codecov.yml also ignores generated code.
set -euo pipefail

# The root module and every published module, less the deprecated cmd/bide-view. The list comes
# from release.sh, whose classification check (run by CI) fails when a module is in neither its
# published nor its repo-only list, so a new published module cannot be left out of coverage.
here=$(cd "$(dirname "$0")" && pwd)
MODULES=". $("$here/release.sh" --list-published | grep -vx 'cmd/bide-view' | tr '\n' ' ')"

[ "${1:-}" = --list ] && { echo $MODULES | tr ' ' '\n'; exit; }

out=${1:-coverage.out}
root=$(cd "$(dirname "$0")/.." && pwd)
case $out in /*) ;; *) out=$PWD/$out ;; esac
mkdir -p "$(dirname "$out")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

( cd "$root" && GOWORK=off go build -o "$tmp/covmerge" ./internal/tools/covmerge )

n=0
for m in $MODULES; do
  echo "== coverage $m =="
  n=$((n + 1))
  ( cd "$root/$m" && GOWORK=off go test -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$tmp/$n.out" ./... )
done

"$tmp/covmerge" -o "$out" "$tmp"/*.out
go tool cover -func="$out" | tail -1
