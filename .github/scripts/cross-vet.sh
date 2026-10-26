#!/usr/bin/env bash
# cross-vet.sh GOOS: go vet every module of the repository for GOOS, on a Linux runner. Nothing is run:
# vet type-checks the packages and their tests for that system, which is what finds a syscall, a
# constant or a build tag that exists on the systems the author uses and not on another.
#
# $MODULES is the module list of ci.yml. The job runs check-modules.sh first, so a module missing from
# the list fails there, and this script also fails on a listed directory that is not a module or a
# list that is empty.
#
# Four systems cannot build the SQLite driver (modernc.org/libc has no files for dragonfly, solaris and
# aix, and illumos lacks symbols). A package that imports it, directly or through another package,
# cannot be type-checked there, and that is not bide's code: on those systems the script vets
# every package that does not import modernc.org/libc (tests included) and says how many it left out.
# The set is derived from `go list`, never listed by hand. It fails loudly when
#   - the derivation finds no package importing modernc.org/libc anywhere (the sqlite modules are in
#     the tree, so the listing broke), or
#   - one of the packages it leaves out type-checks on this system after all (modernc.org/libc now
#     supports it: remove the system from NO_LIBC), or
#   - a go command fails for any other reason.
#
#   .github/scripts/cross-vet.sh linux
#   .github/scripts/cross-vet.sh --self-test   prove the failures above are caught
set -euo pipefail

NO_LIBC="dragonfly solaris illumos aix"
LIBC=modernc.org/libc

arch_of() { case "$1" in aix) echo ppc64 ;; js) echo wasm ;; *) echo amd64 ;; esac; }

# without_libc DIR: the packages of the module in DIR (import paths) that do not import $LIBC, one per
# line, then a line "left out: N" on fd 3 is not used; the count goes to $LEFT_OUT_FILE.
# Each package counts as imported if the package itself, its test variants or its external tests do.
split_packages() { # dir -> writes clean packages to $2, tainted packages to $3
  local dir=$1 clean=$2 tainted=$3 listing
  listing=$(cd "$dir" && go list -test -f '{{.ImportPath}} {{range .Deps}}{{.}} {{end}}' ./...)
  awk -v libc="$LIBC" -v clean="$clean" -v tainted="$tainted" '
    {
      p = $1; sub(/ .*/, "", p)
      sub(/ \[.*\]$/, "", p); sub(/\.test$/, "", p); sub(/_test$/, "", p)
      seen[p] = 1
      for (i = 2; i <= NF; i++) if ($i == libc) bad[p] = 1
    }
    END {
      for (p in seen) print p > (bad[p] ? tainted : clean)
    }' <<< "$listing"
  : >> "$clean"; : >> "$tainted"
}

vet_system() { # GOOS
  local os=$1 arch m dir total_tainted=0 probe="" probe_dir="" vetted=0
  arch=$(arch_of "$os")
  [ -n "${MODULES:-}" ] || { echo "::error::MODULES is empty"; return 1; }
  export GOOS=$os GOARCH=$arch CGO_ENABLED=0
  local no_libc=false
  case " $NO_LIBC " in *" $os "*) no_libc=true ;; esac
  for m in $MODULES; do
    dir=$m
    [ -f "$dir/go.mod" ] || { echo "::error::module $m is in MODULES but $dir/go.mod does not exist"; return 1; }
    if ! $no_libc; then
      echo "== vet $m ($os/$arch)"
      (cd "$dir" && go vet ./...)
      vetted=$((vetted + 1))
      continue
    fi
    local clean tainted
    clean=$(mktemp); tainted=$(mktemp)
    # The listing and the exclusion use this system's own dependency graph with the host's default
    # settings: the sqlite driver is imported the same way on every system.
    (unset GOOS GOARCH CGO_ENABLED; split_packages "$dir" "$clean" "$tainted")
    local n_clean n_tainted
    n_clean=$(wc -l < "$clean" | tr -d ' '); n_tainted=$(wc -l < "$tainted" | tr -d ' ')
    total_tainted=$((total_tainted + n_tainted))
    if [ "$n_tainted" -gt 0 ] && [ -z "$probe" ]; then probe=$(head -1 "$tainted"); probe_dir=$dir; fi
    if [ "$n_clean" -gt 0 ]; then
      echo "== vet $m ($os/$arch): $n_clean packages; left out: $n_tainted that import $LIBC"
      # shellcheck disable=SC2046
      (cd "$dir" && go vet $(cat "$clean"))
      vetted=$((vetted + 1))
    else
      echo "== vet $m ($os/$arch): no package to vet; left out: $n_tainted that import $LIBC"
    fi
    rm -f "$clean" "$tainted"
  done
  if $no_libc; then
    if [ "$total_tainted" -eq 0 ]; then
      echo "::error::no package imports $LIBC in any module, but the sqlite modules are in the tree: the listing is broken, and nothing would be left out"
      return 1
    fi
    # The exclusion must still be needed: a package it leaves out that vets clean means $LIBC supports
    # this system now.
    if (cd "$probe_dir" && go vet "$probe") > /dev/null 2>&1; then
      echo "::error::$probe imports $LIBC and type-checks on $os: the exclusion is no longer needed; remove $os from NO_LIBC in cross-vet.sh"
      return 1
    fi
    echo "left out $total_tainted packages that import $LIBC; $probe still does not type-check on $os"
  fi
  echo "vetted $vetted modules for $os/$arch"
}

self_test() {
  local tmp fail=false
  tmp=$(mktemp -d); trap "rm -rf '$tmp'" EXIT
  # A stand-in for modernc.org/libc: a module of that path, replaced by a local directory, that builds
  # only on linux and darwin, as the real one does not on the other systems.
  mkdir -p "$tmp/libc" "$tmp/uses" "$tmp/plain"
  printf 'module %s\n\ngo 1.27\n' "$LIBC" > "$tmp/libc/go.mod"
  printf '//go:build linux || darwin\n\npackage libc\n\nconst X = 1\n' > "$tmp/libc/libc.go"
  printf 'module example.com/uses\n\ngo 1.27\n\nrequire %s v0.0.0\n\nreplace %s => ../libc\n' "$LIBC" "$LIBC" > "$tmp/uses/go.mod"
  printf 'package uses\n\nimport _ "%s"\n' "$LIBC" > "$tmp/uses/uses.go"
  mkdir -p "$tmp/uses/clean"
  printf 'package clean\n\nconst Y = 2\n' > "$tmp/uses/clean/clean.go"
  printf 'module example.com/plain\n\ngo 1.27\n' > "$tmp/plain/go.mod"
  printf 'package plain\n\nconst Z = 3\n' > "$tmp/plain/plain.go"
  local out="$tmp/out"
  ok_case() { # name, expected exit (0 or 1), grep pattern or "", command...
    local name=$1 want=$2 pat=$3; shift 3
    local rc=0
    ( cd "$tmp" && "$@" ) > "$out" 2>&1 || rc=$?
    local got=0; [ "$rc" -ne 0 ] && got=1
    if [ "$got" -ne "$want" ] || { [ -n "$pat" ] && ! grep -q -- "$pat" "$out"; }; then
      echo "self-test: $name: exit $rc (want ${want}), pattern '$pat'"; sed 's/^/    /' "$out"; fail=true
    fi
  }
  local self; self=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cross-vet.sh
  # 1. On a system with libc, everything vets.
  ok_case "linux vets all" 0 "vetted 2 modules" env MODULES="uses plain" "$self" linux
  # 2. On solaris the package importing libc is left out, the rest vetted, the probe still fails.
  ok_case "solaris leaves out the importer" 0 "left out 1 packages that import $LIBC" env MODULES="uses plain" "$self" solaris
  # 3. A module that vanished from the tree fails loudly.
  ok_case "missing module" 1 "does not exist" env MODULES="uses gone" "$self" linux
  # 4. An empty list fails.
  ok_case "empty list" 1 "MODULES is empty" env MODULES="" "$self" linux
  # 5. No importer found: the exclusion would match nothing.
  ok_case "no importer" 1 "listing is broken" env MODULES="plain" "$self" solaris
  # 6. libc supports the system after all: the exclusion is stale.
  printf 'package libc\n\nconst X = 1\n' > "$tmp/libc/libc.go"
  ok_case "stale exclusion" 1 "no longer needed" env MODULES="uses plain" "$self" solaris
  # 7. A break in a package that is vetted is caught: plain uses a name no system defines.
  printf '//go:build linux || darwin\n\npackage libc\n\nconst X = 1\n' > "$tmp/libc/libc.go"
  printf 'package plain\n\nconst Z = undefinedName\n' > "$tmp/plain/plain.go"
  ok_case "a break in plain code" 1 "undefinedName" env MODULES="uses plain" "$self" solaris
  $fail && return 1
  echo "self-test: ok"
}

case "${1:-}" in
  --self-test) self_test ;;
  "") echo "usage: cross-vet.sh GOOS | --self-test" >&2; exit 2 ;;
  *) vet_system "$1" ;;
esac
