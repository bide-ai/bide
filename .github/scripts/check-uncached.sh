#!/usr/bin/env bash
# check-uncached.sh: every package whose tests run the go tool for more than "go list" and "go mod"
# (go build, go run, go install, ...), or run node, bash or sh over files of the tree, is in
# .github/uncached-tests.txt, and every entry of that file is a package directory with such tests.
#
# go test reuses an unchanged package's result, and decides "unchanged" from the test binary and the
# files and environment variables the test process itself reads. A test that builds or runs another
# package through the go tool is not covered: the program it runs can change while its own
# inputs do not, and the cached pass would stand. The listed packages always run (CI passes
# -count=1 for them, see shards.sh run). The architecture tests that run "go list" and "go mod edit"
# are cached: go.mod, go.sum and go.work changes clear the test cache (gocache.sh).
#
#   .github/scripts/check-uncached.sh             check the tree
#   .github/scripts/check-uncached.sh --self-test prove a missing and a stale entry fail
set -euo pipefail



# needed: the directories of the test files that run the go tool for more than go list / go mod.
needed() {
  git grep -nE 'exec\.Command(Context)?\(([a-zA-Z.]+, *)?("go"|node|"node"|"bash"|"sh")|"build", *"-o"' -- '*_test.go' |
    grep -vE 'exec\.Command(Context)?\(([a-zA-Z.]+, *)?"go", *"(list|mod)"' |
    grep -vE 'LookPath' |
    cut -d: -f1 | xargs -n1 dirname | LC_ALL=C sort -u
}

check() {
  local want have ok=0 d list=${UNCACHED:-.github/uncached-tests.txt}
  want=$(needed)
  have=$(grep -vE '^(#|$)' "$list" | LC_ALL=C sort -u)
  for d in $want; do
    grep -qxF -- "$d" <<< "$have" || { echo "::error::tests in $d run the go tool (build, run, ...): add $d to $list"; ok=1; }
  done
  for d in $have; do
    grep -qxF -- "$d" <<< "$want" || { echo "::error::$list names $d, whose tests do not run the go tool: remove it"; ok=1; }
  done
  [ $ok = 0 ] && echo "uncached test list: ok ($(wc -l <<< "$want" | tr -d ' ') packages)"
  return $ok
}

if [ "${1:-}" = --self-test ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cd "$tmp" && git init -q . && mkdir a b
  printf 'package a\nimport "os/exec"\nfunc f() { exec.Command("go", "build", "-o", "x", ".") }\n' > a/x_test.go
  printf 'package b\nimport "os/exec"\nfunc f() { exec.Command("go", "list", "-deps") }\n' > b/x_test.go
  git add -A
  export UNCACHED=$tmp/list
  printf 'a\n' > "$UNCACHED"; check > /dev/null || { echo "self-test: a correct list failed"; exit 1; }
  printf '' > "$UNCACHED"; check > /dev/null 2>&1 && { echo "self-test: a missing entry passed"; exit 1; }
  printf 'a\nb\n' > "$UNCACHED"; check > /dev/null 2>&1 && { echo "self-test: a stale entry passed"; exit 1; }
  echo "self-test: ok"; exit 0
fi
check
