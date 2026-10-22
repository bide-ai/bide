#!/usr/bin/env bash
# windows-decide.sh: prints "windows=true" or "windows=false", whether the run needs Test
# (windows-latest). The Changes job's windows step runs it in the root of the tree. It reads
#   EVENT  the event name (push, pull_request, merge_group, ...)
#   CODE   the Changes job's code output ("false" when only documentation changed)
#   HEAVY  the Changes job's heavy output (code and full: everything runs)
#   BASE, HEAD  the pull request's base and head, for the windows-needed.sh narrowing
# Only a run that is known to need no Windows job says false: one that changes no code, whatever
# the event (a docs-only push to main or merge queue batch has no Go job to run), or a pull request
# that changes code without Windows-sensitive files (windows-needed.sh). Any doubt (CODE unset or
# not "false", a failed diff, an unexpected event) says true.
#
# --self-test runs the cases in a scratch repository.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [ "${1:-}" = --self-test ]; then
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  cp "$here/windows-needed.sh" "$here/changed-code.sh" "$here/windows-decide.sh" "$tmp/"
  mkdir -p "$tmp/repo/.github/scripts" && cp "$tmp"/*.sh "$tmp/repo/.github/scripts/"
  cd "$tmp/repo"
  git init -q
  git config user.email t@example.com && git config user.name t
  mkdir pure docs
  printf 'package pure\n\nimport "fmt"\n' > pure/pure.go
  printf 'x\n' > docs/a.md
  git add -A && git commit -qm base && base=$(git rev-parse HEAD)
  printf 'package pure\n\nimport "fmt"\n\nvar X = 1\n' > pure/pure.go
  git commit -qam pure && pure=$(git rev-parse HEAD)
  printf 'package pure\n\nimport "fmt"\n\nvar X = 1\n' > pure/pure_test.go
  git add -A && git commit -qm test && sensitive=$(git rev-parse HEAD)
  t() { # t WANT NAME EVENT CODE HEAVY BASE HEAD
    local want=$1 name=$2 got
    got=$(EVENT=$3 CODE=$4 HEAVY=$5 BASE=$6 HEAD=$7 ./.github/scripts/windows-decide.sh) || got="exit $?"
    if [ "$got" = "windows=$want" ]; then echo "PASS $name: $got"; else echo "FAIL $name: $got, want windows=$want"; fail=1; fi
  }
  fail=0
  t false "push, docs only"                 push         false false "" ""
  t true  "push, code"                      push         true  true  "" ""
  t false "pull request, docs only"         pull_request false false "$base" "$base"
  t true  "pull request, sensitive files"   pull_request true  false "$base" "$sensitive"
  t false "pull request, other code"        pull_request true  false "$base" "$pure"
  t false "merge group, docs only"          merge_group  false false "$base" "$base"
  t true  "merge group, code"               merge_group  true  true  "$base" "$sensitive"
  t true  "pull request, failed diff"       pull_request true  false "nosuchsha" "$pure"
  t true  "pull request, code unset"        pull_request ""    false "$base" "$pure"
  t true  "push, code unset"                push         ""    ""    "" ""
  t true  "push, code, heavy unset"         push         true  ""    "$base" "$pure"
  t true  "merge group, code, heavy unset"  merge_group  true  ""    "$base" "$pure"
  t true  "pull request, full-ci label"     pull_request true  true  "$base" "$pure"
  [ "$fail" = 0 ] || { echo "self-test: failed"; exit 1; }
  echo "self-test: ok"; exit 0
fi

windows=true
if [ "${CODE:-}" = false ]; then
  windows=false
elif [ "${CODE:-}" = true ] && [ "${HEAVY:-}" != true ] && [ "${EVENT:-}" = pull_request ]; then
  if files=$(git diff --name-only "${BASE:-}" "${HEAD:-}" 2>/dev/null); then
    .github/scripts/windows-needed.sh <<< "$files" || windows=false
  fi
fi
echo "windows=$windows"
