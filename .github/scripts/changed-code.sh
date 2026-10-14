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
# With --examples-only it exits 0 only if the code paths all lie under examples/ (the example
# modules, which no other module imports), and 1 otherwise.
set -euo pipefail

files=$(cat)
if [ -z "$files" ]; then
  [ "${1:-}" = --examples-only ] && exit 1
  exit 0
fi
# The TLA+ models have their own workflow (models.yml), which decides which of them to check.
other=$(echo "$files" | grep -vE '^(docs/|docs-site/|assets/|spec/tla/|\.github/ISSUE_TEMPLATE/)|\.md$|^(LICENSE|NOTICE|DCO)$|^\.github/workflows/(docs|bench|models|explore)\.yml$' || true)
if [ "${1:-}" = --examples-only ]; then
  [ -n "$other" ] && ! echo "$other" | grep -qvE '^examples/'
  exit
fi
if [ -n "$other" ]; then
  echo "$other"
  exit 0
fi
exit 1
