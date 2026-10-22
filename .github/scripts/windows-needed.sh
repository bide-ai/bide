#!/usr/bin/env bash
# windows-needed.sh: reads the changed files of a pull request on stdin, one per line, and exits 0
# if the pull request should run Test (windows-latest), 1 if it can skip it. It runs in the root of
# the pull request's tree.
#
# The Windows-only failures found in the merge queue lived in a few kinds of code: line endings
# of goldens and testdata, MIME types, os.SameFile, signals in tests. So a pull request runs
# Windows when any changed code file (changed-code.sh: not documentation) is
#   - not a Go file (testdata, goldens, .gitattributes, go.mod, scripts, workflows, ...),
#   - a _test.go file,
#   - a Go file that is gone or that imports os, path, path/filepath, io/fs, syscall, mime,
#     encoding/..., golang.org/x/sys/... (matched as a quoted import path anywhere in the file,
#     which can only over-match),
# and in any case of doubt (no input, an unreadable file). Documentation-only changes need no Go
# job at all (the Changes job's code output), so they skip it too.
#
# --self-test runs the cases in a scratch tree.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
imports='"(os|os/[a-z0-9/]+|path|path/filepath|io/fs|syscall|mime|mime/[a-z0-9/]+|encoding|encoding/[a-z0-9/]+|golang\.org/x/sys[^"]*)"'

if [ "${1:-}" = --self-test ]; then
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  cd "$tmp"
  mkdir -p pure uses testdata docs
  printf 'package pure\n\nimport "fmt"\n' > pure/pure.go
  printf 'package uses\n\nimport (\n\t"fmt"\n\t"path/filepath"\n)\n' > uses/uses.go
  printf 'package uses\n\nimport (\n\t"fmt"\n\tf "os/exec"\n)\n' > uses/exec.go
  printf 'package uses\n\nimport "encoding/json"\n' > uses/enc.go
  printf 'package pure\n\nimport "testing"\n' > pure/pure_test.go
  : > testdata/a.golden
  : > docs/a.md
  t() { # t WANT FILES...: the exit status for the files (one per argument)
    local want=$1 got
    shift
    if printf '%s\n' "$@" | "$here/windows-needed.sh" > /dev/null; then got=0; else got=1; fi
    [ "$got" = "$want" ] || { echo "self-test: $*: exit $got, want $want"; exit 1; }
  }
  t 1 pure/pure.go
  t 1 pure/pure.go docs/a.md CHANGELOG.md
  t 1 docs/a.md
  t 0 uses/uses.go
  t 0 uses/exec.go
  t 0 uses/enc.go
  t 0 pure/pure.go uses/uses.go
  t 0 pure/pure_test.go
  t 0 testdata/a.golden
  t 0 agent/testdata/b.json
  t 0 .gitattributes
  t 0 agent/.gitattributes
  t 0 go.mod
  t 0 go.work
  t 0 .github/workflows/ci.yml
  t 0 scripts/release.sh
  t 0 pure/deleted.go
  t 0 ""
  echo "self-test: ok"; exit 0
fi

files=$(cat)
[ -n "$files" ] || exit 0
code=$(echo "$files" | "$here/changed-code.sh") || exit 1 # documentation only
while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    *_test.go) exit 0 ;;
    *.go) [ -f "$f" ] || exit 0
          if grep -qE "$imports" "$f"; then exit 0; fi ;;
    *) exit 0 ;;
  esac
done <<< "$code"
exit 1
