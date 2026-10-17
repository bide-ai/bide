#!/usr/bin/env bash
# check-pinned-actions.sh: every third-party action a workflow or composite action uses is pinned by a
# full commit SHA, with the tag it stands for in a comment.
#
# A tag such as actions/checkout@v4 can be moved by whoever controls that repository, so a workflow
# that names a tag runs whatever the tag points at today. A commit SHA cannot be moved. Local actions
# (uses: ./.github/actions/...) and container images (uses: docker://...) are not checked here.
#
#   .github/scripts/check-pinned-actions.sh [DIR]        check DIR (default .github)
#   .github/scripts/check-pinned-actions.sh --self-test  prove each failure is caught
set -euo pipefail

# check DIR: prints each unpinned or uncommented reference and fails if there is one.
check() {
  local dir=$1 bad=0 file line ref
  while IFS= read -r hit; do
    file=${hit%%:*}
    rest=${hit#*:}
    line=${rest%%:*}
    ref=$(sed -E 's/.*uses:[[:space:]]*//; s/[[:space:]]+#.*//; s/[[:space:]]*$//' <<<"${rest#*:}")
    case $ref in
      ./* | docker://*) continue ;;
    esac
    if ! [[ $ref =~ @[0-9a-f]{40}$ ]]; then
      echo "$file:$line: $ref is not pinned by a full commit SHA (use owner/repo@<sha> # <tag>)"
      bad=1
    elif ! grep -qE "uses:[[:space:]]*[^[:space:]]+[[:space:]]+#[[:space:]]*[^[:space:]]" <<<"${rest#*:}"; then
      echo "$file:$line: $ref names no tag in a comment (add # <tag>)"
      bad=1
    fi
  done < <(grep -rnE '^[[:space:]-]*uses:' "$dir" --include='*.yml' --include='*.yaml' || true)
  return $bad
}

self_test() {
  local tmp ok=true
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/w"
  expect() { # expect WANT DESCRIPTION LINE
    printf 'jobs:\n  a:\n    steps:\n      %s\n' "$3" > "$tmp/w/x.yml"
    if check "$tmp/w" >/dev/null; then got=pass; else got=fail; fi
    [ "$got" = "$1" ] || { echo "self-test: $2: got $got, want $1"; ok=false; }
  }
  local sha=0123456789abcdef0123456789abcdef01234567
  expect pass "a pinned action with its tag" "- uses: actions/checkout@$sha # v4"
  expect pass "a pinned sub-action" "- uses: actions/cache/save@$sha # v4"
  expect pass "a local action" "- uses: ./.github/actions/go-cache"
  expect pass "a container image" "- uses: docker://alpine:3.20"
  expect fail "a tag" "- uses: actions/checkout@v4"
  expect fail "a branch" "- uses: actions/checkout@main"
  expect fail "a short SHA" "- uses: actions/checkout@0123456 # v4"
  expect fail "a pinned action with no tag comment" "- uses: actions/checkout@$sha"
  expect fail "an uppercase SHA" "- uses: actions/checkout@0123456789ABCDEF0123456789ABCDEF01234567 # v4"
  $ok && echo "check-pinned-actions self-test: ok"
  $ok
}

case ${1:-} in
  --self-test) self_test ;;
  *)
    check "${1:-.github}"
    echo "check-pinned-actions: ok"
    ;;
esac
