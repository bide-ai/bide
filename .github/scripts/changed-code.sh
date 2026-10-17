#!/usr/bin/env bash
# changed-code.sh: read the changed file paths on stdin and decide whether any is code. Exits 0
# (printing the code paths) if one is, or if the list is empty (no files found is a doubt, and a
# doubt counts as code); exits 1 if every path is documentation (Markdown at the top level or under
# docs/, changes/ and .github/, and images under docs/ and assets/), docs-site/, a TLA+
# specification or an issue template, which no Go build or test reads.
#
# A path is code, whatever its directory, when it is a Go file, a go.mod, go.sum or go.work file,
# anything under a testdata directory, a Markdown file inside a package (a test or go:embed may read
# it), or, under docs/ and assets/, a file that is not a document or an image (a script, a
# program, JSON). An unlisted extension is code: the list of safe ones is short on purpose.
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
  t 0 "" docs/reference/bide-anchor.md
  t 0 "" docs/reference/api/agent.md
  t 0 "" docs/trust/stability.md
  t 0 "" docs/releases/v0.17.0.md
  t 1 "" docs/reference/module-structure.md
  t 0 "" docs/guide.md agent/run.go
  t 0 "" ""
  t 0 --examples-only examples/plan/main.go
  t 1 --examples-only examples/plan/main.go agent/run.go
  t 1 --examples-only docs/reference/cli.md
  t 1 --examples-only docs/reference/api/index.md
  t 1 --examples-only docs/guide.md
  t 0 --gsm agent/run.go
  t 1 --gsm docs/guide.md
  t 0 --gsm docs/reference/cli.md
  # Go files and the files tests read count as code wherever they are (assets/demo/setup/deadline.go
  # was merged unformatted as a documentation-only change).
  t 0 "" assets/demo/setup/deadline.go
  t 0 "" assets/demo/browser-shots/intake/main.go docs/guide.md
  t 0 "" docs/guides/snippet.go
  t 0 "" docs-site/x.go
  t 0 "" spec/tla/claims/x.go
  t 0 "" docs/go.mod
  t 0 "" docs-site/testdata/x.json
  t 0 "" spec/tla/testdata/a.cfg
  t 0 "" assets/demo/go.sum
  t 0 "" go.work
  t 0 "" internal/tools/docsnip/testdata/ok.md
  t 0 "" audit/rfc3161/testdata/README.md
  t 0 "" benchmarks/README.md
  t 0 "" cmd/bide/internal/x/page.md
  t 0 "" docs/guides/data.json
  t 0 "" assets/demo/shots-deadline.sh
  t 1 "" docs/guides/img/a.png assets/demo/shot.svg docs/i18n/ru/README.md
  t 1 "" CHANGELOG.md changes/unreleased/x.md .github/pull_request_template.md .github/gsm-gate/README.md
  t 1 "" docs-site/config.mjs docs-site/package.json spec/tla/claims/Claims.tla spec/tla/check.sh .github/ISSUE_TEMPLATE/bug.yml
  t 0 --gsm assets/demo/setup/deadline.go
  t 0 --examples-only examples/plan/README.md
  t 1 --examples-only assets/demo/setup/deadline.go
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
docs_re='^(docs/|docs-site/|assets/|spec/tla/|\.github/ISSUE_TEMPLATE/)|^[^/]*\.md$|^(changes|\.github)/.*\.md$|^(LICENSE|NOTICE|DCO)$|^\.github/workflows/(docs|bench|models|explore)\.yml$'
other=$(echo "$files" | grep -vE "$docs_re" || true)
# Paths that look like documentation but that Go builds, tests or tools read.
docs_like=$(echo "$files" | grep -E "$docs_re" || true)
codeish=$( { echo "$docs_like" | grep -E '\.go$|(^|/)go\.(mod|sum|work|work\.sum)$|(^|/)testdata/' || true
             echo "$docs_like" | grep -E '^(docs|assets)/' | grep -vE '\.(md|png|jpe?g|gif|svg|webp|avif|ico|txt|mp4|webm|pdf|cast)$' || true; } | grep -v '^$' || true)
if [ -n "$codeish" ]; then
  other=$(printf '%s\n%s\n' "$other" "$codeish" | grep -v '^$' | LC_ALL=C sort -u)
fi
# Documents that Go tests compare with the code count as code: the command reference tests
# (cmd/bide, cmd/bide/bide-hsm, anchor/awss3/cmd/bide-anchor reference_test.go) fail when docs/reference/cli.md, bide-hsm.md or bide-anchor.md
# differs from the commands' help, TestAPIReference (internal/tools/apiref) fails when
# docs/reference/api/ or the package table in docs/trust/stability.md differs from the sources, and
# doccheck and TestRepoReleaseNotes read docs/releases/.
tested=$(echo "$files" | grep -E '^docs/(reference/(cli|bide-hsm|bide-anchor)\.md|reference/api/|trust/stability\.md|releases/)' || true)
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
