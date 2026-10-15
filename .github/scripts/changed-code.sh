#!/usr/bin/env bash
# changed-code.sh: read the changed file paths on stdin and decide whether any is code. Exits 0
# (printing the code paths) if one is, or if the list is empty (no files found is a doubt, and a
# doubt counts as code); exits 1 if every path is documentation, an asset, a TLA+ specification
# or an issue template, which no Go build or test reads.
#
# Used by ci.yml (the Changes job) and gsm-gate.yml to skip work for documentation-only pull
# requests.
#
#   git diff --name-only BASE HEAD | .github/scripts/changed-code.sh
#
# With --gsm it exits 0 if a path can change what a bide program makes (the gsm machine gate):
# code, less the CI scripts and the other workflows, scripts/ and tool configuration.
#
# With --examples-only it exits 0 only if the code paths all lie under examples/ (the example
# modules, which no other module imports), and 1 otherwise.
set -euo pipefail

if [ "${1:-}" = --self-test ]; then
  me=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
  t() { # t WANT FLAG FILES...: the exit status for the files (one per argument)
    local want=$1 flag=$2 got
    shift 2
    if printf '%s\n' "$@" | "$me" ${flag:+"$flag"} > /dev/null; then got=0; else got=1; fi
    [ "$got" = "$want" ] || { echo "self-test: $flag $*: exit $got, want $want"; exit 1; }
  }
  t 1 "" docs/guide.md README.md docs-site/x.mjs spec/tla/claims/Claims.tla
  t 0 "" agent/run.go
  t 0 "" docs/reference/cli.md
  t 0 "" docs/reference/bide-hsm.md
  t 0 "" docs/releases/v0.17.0.md
  t 1 "" docs/reference/module-structure.md
  t 0 "" docs/guide.md agent/run.go
  t 0 "" ""
  t 0 --examples-only examples/plan/main.go
  t 1 --examples-only examples/plan/main.go agent/run.go
  t 1 --examples-only docs/reference/cli.md
  t 1 --examples-only docs/guide.md
  t 0 --gsm agent/run.go
  t 1 --gsm docs/guide.md
  t 0 --gsm docs/reference/cli.md
  t 1 --gsm .github/workflows/ci.yml scripts/x.sh
  t 0 --gsm .github/gsm-gate/machines.txt
  echo "self-test: ok"; exit 0
fi

files=$(cat)
if [ -z "$files" ]; then
  [ "${1:-}" = --examples-only ] && exit 1
  exit 0
fi
# The TLA+ models have their own workflow (models.yml), which decides which of them to check.
other=$(echo "$files" | grep -vE '^(docs/|docs-site/|assets/|spec/tla/|\.github/ISSUE_TEMPLATE/)|\.md$|^(LICENSE|NOTICE|DCO)$|^\.github/workflows/(docs|bench|models|explore)\.yml$' || true)
# Documents that Go tests compare with the code count as code: the command reference tests
# (cmd/bide, cmd/bide/bide-hsm reference_test.go) fail when docs/reference/cli.md or bide-hsm.md
# differs from the commands' help, and doccheck and TestRepoReleaseNotes read docs/releases/.
tested=$(echo "$files" | grep -E '^docs/(reference/(cli|bide-hsm)\.md|releases/)' || true)
if [ -n "$tested" ]; then
  other=$(printf '%s\n%s\n' "$other" "$tested" | grep -v '^$' | LC_ALL=C sort -u)
fi
if [ "${1:-}" = --gsm ]; then
  rest=$(echo "$other" | grep -vE '^(\.github/(scripts/|workflows/)|scripts/|codecov\.yml$|\.gitignore$|\.gitattributes$)' || true)
  # the gate's own workflow and catalog are inputs of the gate
  gate=$(echo "$files" | grep -E '^\.github/(gsm-gate/|workflows/gsm-gate\.yml$)' || true)
  [ -n "$rest" ] || [ -n "$gate" ]
  exit
fi
if [ "${1:-}" = --examples-only ]; then
  [ -n "$other" ] && ! echo "$other" | grep -qvE '^examples/'
  exit
fi
if [ -n "$other" ]; then
  echo "$other"
  exit 0
fi
exit 1
