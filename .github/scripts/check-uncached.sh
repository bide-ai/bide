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

# owners FILE: the package directories whose tests run the process-starting FILE: its own package,
# and for a non-test file with no tests of its own, the packages whose tests import it (an import
# path ends with the directory).
owners() {
  local f=$1 d imp
  d=$(dirname "$f")
  case "$f" in
    *_test.go) echo "$d" ;;
    *)
      if git ls-files -- "$d/*_test.go" | grep -qv "^$d/.*/"; then echo "$d"
      else for imp in $(git grep -lE "/$d\"" -- '*_test.go' || true); do dirname "$imp"; done
      fi ;;
  esac
}

check() {
  local unc=${UNCACHED:-.github/uncached-tests.txt} sound=${SOUND:-.github/cache-sound-tests.txt}
  local procs own="" ok=0 f d u s bad
  # Every file that starts a process, test or not: a non-test file runs when its package's tests do.
  procs=$(git grep -lE 'exec\.Command(Context)?\(' -- '*.go' | LC_ALL=C sort -u || true)
  u=$(grep -vE '^(#|$)' "$unc" | LC_ALL=C sort -u)
  s=$(grep -vE '^(#|$)' "$sound" | sed 's/:.*//' | LC_ALL=C sort -u)
  for f in $procs; do
    local owned=true
    for d in $(owners "$f" | LC_ALL=C sort -u); do
      own+=$d$'\n'
      grep -qxF -- "$d" <<< "$u" && continue
      if grep -qxF -- "$f" <<< "$s"; then
        if bad=$(grep -nE '"\./\.\.\."|/\.\.\.|TestImports' "$f"); then
          echo "::error::$f is in $sound but lists packages ($(head -1 <<< "$bad")): add $d to $unc instead"; ok=1
        fi
      else
        echo "::error::$f starts a process (exec.Command), run by the tests of $d: add $d to $unc, or the file to $sound with the reason its result depends only on the test binary"; ok=1
      fi
    done
  done
  own=$(printf '%s' "$own" | LC_ALL=C sort -u)
  for d in $u; do
    grep -qxF -- "$d" <<< "$own" || { echo "::error::$unc names $d, whose tests run no process-starting file: remove it"; ok=1; }
  done
  for f in $s; do
    grep -qxF -- "$f" <<< "$procs" || { echo "::error::$sound names $f, which starts no process: remove it"; ok=1; continue; }
    for d in $(owners "$f"); do
      grep -qxF -- "$d" <<< "$u" && { echo "::error::$sound names $f, whose package $d is in $unc: remove it from $sound"; ok=1; }
    done
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
  mkdir -p d e/lib
  printf 'package d\nimport "os/exec"\nfunc F() { exec.Command("go", "list") }\n' > d/d.go
  printf 'package d\n' > d/d_test.go
  printf 'package lib\nimport "os/exec"\nfunc F() { exec.Command("go", "doc") }\n' > e/lib/lib.go
  printf 'package e\nimport _ "x/e/lib"\n' > e/e_test.go
  git add -A
  export UNCACHED=$tmp/unc SOUND=$tmp/sound
  t() { # t WANT UNCACHED-LINES SOUND-LINES DESCRIPTION
    printf '%b' "$2" > "$UNCACHED"; printf '%b' "$3" > "$SOUND"
    if check > /dev/null 2>&1; then got=pass; else got=fail; fi
    [ "$got" = "$1" ] || { echo "self-test: $4: got $got, want $1"; exit 1; }
  }
  OK_S='b/x_test.go: re-runs the test binary\nd/d.go: lists its own package\n'
  t pass 'a\nc\ne\n' "$OK_S" "every file accounted for"
  t fail 'c\ne\n' "$OK_S" "a build test in no list"
  t fail 'a\nc\ne\n' 'd/d.go: self\n' "a self-exec test in no list"
  t fail 'a\ne\n' "${OK_S}c/x_test.go: go list\n" "a go list ./... test taken as sound"
  t fail 'a\nc\ne\nz\n' "$OK_S" "a stale uncached entry"
  t fail 'a\nc\ne\n' "${OK_S}b/gone_test.go: self\n" "a stale sound entry"
  t fail 'a\nb\nc\ne\n' "$OK_S" "a sound entry whose package is uncached"
  t fail 'a\nc\ne\n' 'b/x_test.go: self\n' "a non-test file that starts a process, in a package with tests, in no list"
  t pass 'a\nc\nd\ne\n' 'b/x_test.go: self\n' "the same package uncached"
  t fail 'a\nc\n' "$OK_S" "a non-test file imported by tests (e/lib via e), in no list"
  echo "self-test: ok"; exit 0
fi
check
