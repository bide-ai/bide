#!/usr/bin/env bash
# check-modules.sh [root]: fails unless every Go module under root is listed in $MODULES and in
# root/go.work, except those named in $UNLISTED_MODULES. A module missing from MODULES would never
# be built or tested by CI, so its tests could not fail.
#
# Like the go tool, it ignores a go.mod inside a directory named testdata or vendor, or whose name
# starts with "." or "_" (such a tree is test input, not a module of this repository), and inside
# node_modules.
#
# check-modules.sh --self-test runs the check against a scratch tree and fails unless a go.mod
# under testdata/, vendor/, node_modules/, .hidden/ or _skip/ is ignored while a real new module
# is still caught (missing from MODULES, and missing from go.work).
set -euo pipefail

modules_in() { # root: every module directory under root, relative, "." for root itself, sorted
  (cd "$1" && find . -mindepth 1 -type d \( -name testdata -o -name vendor -o -name node_modules -o -name '.*' -o -name '_*' \) -prune \
    -o -name go.mod -type f -print) | sed -e 's|^\./||' -e 's|/\{0,1\}go\.mod$||' -e 's|^$|.|' | LC_ALL=C sort
}

check() { # root
  local root=$1 found expected listed workspace ok=true
  found=$(modules_in "$root")
  expected=$(echo "$found" | grep -vxF -f <(tr ' ' '\n' <<< "${UNLISTED_MODULES:-}" | sed '/^$/d') || true)
  listed=$(tr ' ' '\n' <<< "$MODULES" | sed '/^$/d' | LC_ALL=C sort)
  workspace=$(go work edit -json "$root/go.work" | jq -r '.Use[].DiskPath' | sed -e 's|^\./||' -e 's|^$|.|' | LC_ALL=C sort)
  if [ "$expected" != "$listed" ]; then
    echo "::error::MODULES does not match the modules in the tree"; diff <(echo "$expected") <(echo "$listed") || true; ok=false
  fi
  if [ "$expected" != "$workspace" ]; then
    echo "::error::go.work does not match the modules in the tree"; diff <(echo "$expected") <(echo "$workspace") || true; ok=false
  fi
  $ok
}

self_test() {
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  mod() { mkdir -p "$tmp/$1"; printf 'module example.com/%s\n\ngo 1.27\n' "${1//\//_}" > "$tmp/$1/go.mod"; }
  mod . ; mod a ; mod skip
  for d in a/testdata/repo/nested testdata/x vendor/example.com/y node_modules/z .hidden/w _skip/v; do mod "$d"; done
  printf 'go 1.27\n\nuse (\n\t.\n\t./a\n)\n' > "$tmp/go.work"
  local fail=false
  if ! MODULES=". a" UNLISTED_MODULES="skip" check "$tmp" > "$tmp/out" 2>&1; then
    echo "self-test: go.mod files under testdata/vendor/node_modules/./_ were not ignored:"; cat "$tmp/out"; fail=true
  fi
  mod b
  if MODULES=". a" UNLISTED_MODULES="skip" check "$tmp" > "$tmp/out" 2>&1; then
    echo "self-test: a new module b missing from MODULES and go.work was not caught"; fail=true
  fi
  if MODULES=". a b" UNLISTED_MODULES="skip" check "$tmp" > "$tmp/out" 2>&1; then
    echo "self-test: a new module b missing from go.work only was not caught"; fail=true
  fi
  printf 'go 1.27\n\nuse (\n\t.\n\t./a\n\t./b\n)\n' > "$tmp/go.work"
  if ! MODULES=". a b" UNLISTED_MODULES="skip" check "$tmp" > "$tmp/out" 2>&1; then
    echo "self-test: module b listed in both still failed:"; cat "$tmp/out"; fail=true
  fi
  if MODULES=". a b" UNLISTED_MODULES="" check "$tmp" > "$tmp/out" 2>&1; then
    echo "self-test: module skip, no longer exempted, was not caught"; fail=true
  fi
  $fail && return 1
  echo "self-test: ok"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
else
  check "${1:-.}"
fi
