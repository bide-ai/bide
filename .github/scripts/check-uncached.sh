#!/usr/bin/env bash
# check-uncached.sh: every test file that starts another process is accounted for.
#
# go test reuses an unchanged package's result, and decides "unchanged" from the test binary and the
# files and environment variables the test process itself reads. A test that starts another process
# can depend on things go test does not see: another package it builds or lists (go build, go list
# ./...), a script over files of the tree (node, bash), a program named by the environment. A cached
# pass then stands although the thing under test changed. So every _test.go file that calls
# exec.Command or exec.CommandContext must be either
#   - in a package listed in .github/uncached-tests.txt, which CI always runs with -count=1
#     (shards.sh run), or
#   - listed in .github/cache-sound-tests.txt with the reason its result is a function of the test
#     binary and the module files: it re-runs the test binary itself (os.Args[0]), runs git in a
#     temporary directory, runs "go list" on its own package, or runs "go mod edit" (go.mod, go.sum
#     and go.work changes clear the test cache, gocache.sh).
# A file in the second list must not list a pattern ("./...", "/...") or use the TestImports
# templates: those read packages the test binary does not contain; move its package to the first
# list. (A "go list" with a Dir of its own is the reviewer's call: store/postgres is listed for it.)
# Entries that no longer apply fail too.
#
#   .github/scripts/check-uncached.sh             check the tree
#   .github/scripts/check-uncached.sh --self-test prove each failure is caught
set -euo pipefail

check() {
  local unc=${UNCACHED:-.github/uncached-tests.txt} sound=${SOUND:-.github/cache-sound-tests.txt}
  local files dirs ok=0 f d u s
  files=$(git grep -lE 'exec\.Command(Context)?\(' -- '*_test.go' | LC_ALL=C sort -u)
  u=$(grep -vE '^(#|$)' "$unc" | LC_ALL=C sort -u)
  s=$(grep -vE '^(#|$)' "$sound" | sed 's/:.*//' | LC_ALL=C sort -u)
  for f in $files; do
    d=$(dirname "$f")
    if grep -qxF -- "$d" <<< "$u"; then continue; fi
    if grep -qxF -- "$f" <<< "$s"; then
      if bad=$(grep -nE '"\./\.\.\."|/\.\.\.|TestImports' "$f"); then
        echo "::error::$f is in $sound but lists packages ($(head -1 <<< "$bad")): add $d to $unc instead"; ok=1
      fi
      continue
    fi
    echo "::error::$f starts a process (exec.Command): add its package $d to $unc, or the file to $sound with the reason its result depends only on the test binary"; ok=1
  done
  dirs=$(for f in $files; do dirname "$f"; done | LC_ALL=C sort -u)
  for d in $u; do
    grep -qxF -- "$d" <<< "$dirs" || { echo "::error::$unc names $d, whose tests start no process: remove it"; ok=1; }
  done
  for f in $s; do
    grep -qxF -- "$f" <<< "$files" || { echo "::error::$sound names $f, which starts no process: remove it"; ok=1; }
    grep -qxF -- "$(dirname "$f")" <<< "$u" && { echo "::error::$sound names $f, whose package is in $unc: remove it from $sound"; ok=1; }
  done
  [ $ok = 0 ] && echo "test cache lists: ok ($(wc -l <<< "$u" | tr -d ' ') uncached packages, $(wc -l <<< "$s" | tr -d ' ') sound files)"
  return $ok
}

if [ "${1:-}" = --self-test ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  cd "$tmp" && git init -q . && mkdir a b c
  printf 'package a\nimport "os/exec"\nfunc f() { exec.Command("go", "build", "-o", "x", ".") }\n' > a/x_test.go
  printf 'package b\nimport "os/exec"\nfunc f() { exec.Command(os.Args[0]) }\n' > b/x_test.go
  printf 'package c\nimport "os/exec"\nfunc f() { exec.Command("go", "list", "./...") }\n' > c/x_test.go
  git add -A
  export UNCACHED=$tmp/unc SOUND=$tmp/sound
  t() { # t WANT UNCACHED-LINES SOUND-LINES DESCRIPTION
    printf '%b' "$2" > "$UNCACHED"; printf '%b' "$3" > "$SOUND"
    if check > /dev/null 2>&1; then got=pass; else got=fail; fi
    [ "$got" = "$1" ] || { echo "self-test: $4: got $got, want $1"; exit 1; }
  }
  t pass 'a\nc\n' 'b/x_test.go: re-runs the test binary\n' "every file accounted for"
  t fail 'c\n' 'b/x_test.go: self\n' "a build test in no list"
  t fail 'a\nc\n' '' "a self-exec test in no list"
  t fail 'a\n' 'b/x_test.go: self\nc/x_test.go: go list\n' "a go list ./... test taken as sound"
  t fail 'a\nc\nz\n' 'b/x_test.go: self\n' "a stale uncached entry"
  t fail 'a\nc\n' 'b/x_test.go: self\nb/gone_test.go: self\n' "a stale sound entry"
  t fail 'a\nb\nc\n' 'b/x_test.go: self\n' "a sound entry whose package is uncached"
  echo "self-test: ok"; exit 0
fi
check
