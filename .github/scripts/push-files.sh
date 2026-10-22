#!/usr/bin/env bash
# push-files.sh BEFORE SHA [FORCED]: print the files a push to main changed, one per line, for the
# Changes job to classify (changed-code.sh). It answers only for the one case it can trust: a push
# of exactly one commit that sits on top of BEFORE. It exits 1, printing nothing, for everything
# else, and the caller then treats the push as a code change and runs every job:
#   - BEFORE empty or all zeros (a first push, a new branch);
#   - FORCED true (a force push: BEFORE may not be an ancestor of SHA);
#   - BEFORE not in the clone, or not an ancestor of SHA;
#   - more than one commit in BEFORE..SHA (a docs-only last commit says nothing about the others),
#     or none.
# The clone must be deep enough to hold BEFORE (the Changes job checks out with fetch-depth 0).
#
# --self-test builds a scratch repository and runs the cases above.
set -euo pipefail

if [ "${1:-}" = --self-test ]; then
  me=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  cd "$tmp"
  git init -q -b main .
  git config user.email t@example.invalid
  git config user.name t
  mkdir docs agent
  c() { mkdir -p "$(dirname "$1")"; echo "$RANDOM" > "$1"; git add -A; git commit -q -m "$1"; git rev-parse HEAD; }
  base=$(c README.md)
  docs=$(c docs/a.md)
  code=$(c agent/a.go)
  want() { # want NAME STATUS OUTPUT ARGS...
    local name=$1 status=$2 out=$3 got rc=0
    shift 3
    got=$("$me" "$@" 2>/dev/null) || rc=$?
    [ "$rc" = "$status" ] && [ "$got" = "$out" ] || { echo "self-test: $name: exit $rc, output '$got'; want exit $status, output '$out'"; exit 1; }
  }
  want "one docs commit" 0 "docs/a.md" "$base" "$docs"
  want "one code commit" 0 "agent/a.go" "$docs" "$code"
  want "two commits" 1 "" "$base" "$code"
  want "empty before" 1 "" "" "$code"
  want "zero before" 1 "" 0000000000000000000000000000000000000000 "$code"
  want "forced" 1 "" "$docs" "$code" true
  want "forced false" 0 "agent/a.go" "$docs" "$code" false
  want "unknown before" 1 "" deadbeefdeadbeefdeadbeefdeadbeefdeadbeef "$code"
  want "same commit" 1 "" "$code" "$code"
  git checkout -q -b other "$base"
  side=$(c docs/side.md)
  want "not an ancestor" 1 "" "$side" "$code"
  want "reversed" 1 "" "$code" "$docs"
  echo "self-test: ok"; exit 0
fi

before=${1:-} sha=${2:-} forced=${3:-}
case "$before" in ''|*[!0]*) ;; *) exit 1 ;; esac # empty or all zeros
[ -n "$before" ] && [ -n "$sha" ] || exit 1
[ "$forced" != true ] || exit 1
git cat-file -e "$before^{commit}" 2>/dev/null || exit 1
git merge-base --is-ancestor "$before" "$sha" 2>/dev/null || exit 1
[ "$(git rev-list --count "$before..$sha")" = 1 ] || exit 1
git diff --name-only "$before" "$sha"
