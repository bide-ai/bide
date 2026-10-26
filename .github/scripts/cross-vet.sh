#!/usr/bin/env bash
# cross-vet.sh GOOS: go vet every module of the repository for GOOS, on a Linux runner. Nothing is run:
# vet type-checks the packages and their tests for that system, which is what finds a syscall, a
# constant or a build tag that exists on the systems the author uses and not on another.
#
# $MODULES is the module list of ci.yml. The job runs check-modules.sh first, so a module missing from
# the list fails there; this script also fails on a listed directory that is not a module and on an
# empty list.
#
# Six systems cannot build the SQLite driver: modernc.org/libc has no files for dragonfly, solaris, aix,
# plan9 and js, and illumos lacks symbols. Every package that imports modernc.org/sqlite, however far down
# (cmd/bide's main package among them), would then not type-check, and that is not bide's code. So on
# those systems the driver is replaced, by a workspace replace, with an empty package of the same
# import path: the packages that only blank-import it (as store/sqlite does, to register the database/sql
# driver) type-check for real. The packages that use its API (modernc.org/sqlite/lib, in
# govern/sqlitelog) cannot, and are left out; they are found by `go list` (a package or dependency
# with an error), never listed by hand, and the log counts and names them. It fails loudly when
#   - no package imports modernc.org/libc in any module (the sqlite modules are in the tree, so the
#     listing broke), or
#   - a package is left out that does not import modernc.org/libc (it cannot be loaded for another
#     reason: that is a break, not a driver limit), or
#   - a package that imports modernc.org/libc type-checks on this system with the real driver (it supports
#     the system now: remove it from NO_LIBC), or
#   - any other go command fails, or a package that is vetted does not type-check.
#
#   .github/scripts/cross-vet.sh linux
#   .github/scripts/cross-vet.sh --self-test   prove the failures above are caught
set -euo pipefail

NO_LIBC="dragonfly solaris illumos aix plan9 js"
LIBC=modernc.org/libc
DRIVER=modernc.org/sqlite

arch_of() { case "$1" in aix) echo ppc64 ;; js) echo wasm ;; *) echo amd64 ;; esac; }

# list_split REAL_OR_STUB_WORK DIR PATTERN_FILE_PREFIX: reads `go list -e -test` for the module in DIR, with
# the host's settings, and writes, one import path per line,
#   $3.libc  the packages (a package, its test variants and its external tests count as one) that import $LIBC
#   $3.bad   the packages with an error or a dependency with an error
#   $3.all   every package
list_split() {
  local work=$1 dir=$2 prefix=$3 listing
  listing=$(cd "$dir" && GOWORK=$work go list -e -test -f '{{.ImportPath}}{{if or .Error .DepsErrors}} !{{end}} {{range .Deps}}{{.}} {{end}}' ./...)
  : > "$prefix.libc"; : > "$prefix.bad"; : > "$prefix.all"
  awk -v libc="$LIBC" -v prefix="$prefix" '
    {
      p = $1; sub(/ \[.*\]$/, "", p); sub(/\.test$/, "", p); sub(/_test$/, "", p)
      seen[p] = 1
      for (i = 2; i <= NF; i++) { if ($i == "!") bad[p] = 1; if ($i == libc) hasl[p] = 1 }
    }
    END {
      for (p in seen) { print p >> (prefix ".all"); if (hasl[p]) print p >> (prefix ".libc"); if (bad[p]) print p >> (prefix ".bad") }
    }' <<< "$listing"
}

vet_system() { # GOOS
  local os=$1 arch dir m vetted=0
  arch=$(arch_of "$os")
  [ -n "${MODULES:-}" ] || { echo "::error::MODULES is empty"; return 1; }
  for m in $MODULES; do
    [ -f "$m/go.mod" ] || { echo "::error::module $m is in MODULES but $m/go.mod does not exist"; return 1; }
  done
  local no_libc=false
  case " $NO_LIBC " in *" $os "*) no_libc=true ;; esac
  if ! $no_libc; then
    for m in $MODULES; do
      echo "== vet $m ($os/$arch)"
      (cd "$m" && GOOS=$os GOARCH=$arch CGO_ENABLED=0 go vet ./...)
      vetted=$((vetted + 1))
    done
    echo "vetted $vetted modules for $os/$arch"
    return 0
  fi

  # Two workspaces over the same modules: the real driver, and the empty stand-in for it.
  local tmp stub real stubw
  tmp=$(mktemp -d); trap "rm -rf '$tmp'" RETURN
  stub=$tmp/stub; mkdir -p "$stub"
  printf 'module %s\n\ngo 1.27\n' "$DRIVER" > "$stub/go.mod"
  printf 'package sqlite\n' > "$stub/sqlite.go"
  real=$tmp/real.work; stubw=$tmp/stub.work # outside the tree: nothing is left in the checkout
  # The go line is the repository's own (go.work): the modules require it, and a lower one is refused.
  local goline
  goline=$(awk '$1 == "go" { print $2; exit }' go.work 2>/dev/null || true)
  for w in "$real" "$stubw"; do
    { printf 'go %s\n\nuse (\n' "${goline:-1.27}"; for m in $MODULES; do printf '\t%s\n' "$(cd "$m" && pwd)"; done; printf ')\n'; } > "$w"
  done
  printf '\nreplace %s => %s\n' "$DRIVER" "$stub" >> "$stubw"

  local libc_total=0 left_total=0 probe="" probe_dir="" n_clean n_left
  for m in $MODULES; do
    list_split "$real" "$m" "$tmp/real" # the host, the real driver
    list_split "$stubw" "$m" "$tmp/stub" # the host, the stand-in
    libc_total=$((libc_total + $(wc -l < "$tmp/real.libc")))
    # What is left out must import the driver's libc in the real graph: a package that cannot be loaded for
    # another reason is a break.
    while read -r p; do
      [ -n "$p" ] || continue
      if ! grep -qxF "$p" "$tmp/real.libc"; then
        echo "::error::package $p cannot be loaded (with the driver replaced by the stand-in) and does not import $LIBC: that is a break, not a driver limit"
        (cd "$m" && GOWORK=$stubw go list -e -f '{{.Error}} {{.DepsErrors}}' "$p") || true
        return 1
      fi
    done < "$tmp/stub.bad"
    grep -vxF -f "$tmp/stub.bad" "$tmp/stub.all" > "$tmp/clean" || true
    n_clean=$(grep -c . "$tmp/clean" || true); n_left=$(grep -c . "$tmp/stub.bad" || true)
    left_total=$((left_total + n_left))
    if [ -z "$probe" ]; then
      if [ "$n_left" -gt 0 ]; then probe=$(head -1 "$tmp/stub.bad"); probe_dir=$m
      elif [ -s "$tmp/real.libc" ]; then probe=$(head -1 "$tmp/real.libc"); probe_dir=$m; fi
    fi
    echo "== vet $m ($os/$arch): $n_clean packages; left out: $n_left that use the driver's API"
    if [ "$n_left" -gt 0 ]; then sed 's/^/     left out: /' "$tmp/stub.bad"; fi
    if [ "$n_clean" -gt 0 ]; then
      # shellcheck disable=SC2046
      (cd "$m" && GOWORK=$stubw GOOS=$os GOARCH=$arch CGO_ENABLED=0 go vet $(cat "$tmp/clean"))
      vetted=$((vetted + 1))
    fi
  done
  if [ "$libc_total" -eq 0 ]; then
    echo "::error::no package imports $LIBC in any module, but the sqlite modules are in the tree: the listing is broken"
    return 1
  fi
  # The stand-in must still be needed: a package that imports the real driver and type-checks on this system
  # means $LIBC supports it now.
  if (cd "$probe_dir" && GOWORK=$real GOOS=$os GOARCH=$arch CGO_ENABLED=0 go vet "$probe") > /dev/null 2>&1; then
    echo "::error::$probe imports $LIBC and type-checks on $os with the real driver: the stand-in is no longer needed; remove $os from NO_LIBC in cross-vet.sh"
    return 1
  fi
  echo "left out $left_total packages that use the driver's API; $probe still does not type-check on $os with the real driver"
  echo "vetted $vetted modules for $os/$arch"
}

self_test() {
  local tmp fail=false
  tmp=$(mktemp -d); trap "rm -rf '$tmp'" EXIT
  # Stand-ins for the driver and its libc: modules of those paths, replaced by local directories. The libc builds
  # only on linux and darwin, as the real one does not on the other systems.
  mkdir -p "$tmp/libc" "$tmp/sqlite/lib" "$tmp/uses" "$tmp/needslib" "$tmp/plain"
  printf 'module %s\n\ngo 1.27\n' "$LIBC" > "$tmp/libc/go.mod"
  printf '//go:build linux || darwin\n\npackage libc\n\nconst X = 1\n' > "$tmp/libc/libc.go"
  printf 'module %s\n\ngo 1.27\n\nrequire %s v0.0.0\n\nreplace %s => ../libc\n' "$DRIVER" "$LIBC" "$LIBC" > "$tmp/sqlite/go.mod"
  printf 'package sqlite\n\nimport _ "%s"\n\nconst Driver = 1\n' "$LIBC" > "$tmp/sqlite/sqlite.go"
  printf 'package lib\n\nimport _ "%s"\n\nconst L = 1\n' "$LIBC" > "$tmp/sqlite/lib/lib.go"
  local m
  for m in uses needslib; do
    printf 'module example.com/%s\n\ngo 1.27\n\nrequire (\n\t%s v0.0.0\n\t%s v0.0.0\n)\n\nreplace (\n\t%s => ../sqlite\n\t%s => ../libc\n)\n' "$m" "$DRIVER" "$LIBC" "$DRIVER" "$LIBC" > "$tmp/$m/go.mod"
  done
  printf 'package uses\n\nimport _ "%s"\n' "$DRIVER" > "$tmp/uses/uses.go"
  printf 'package needslib\n\nimport "%s/lib"\n\nvar _ = lib.L\n' "$DRIVER" > "$tmp/needslib/needslib.go"
  printf 'module example.com/plain\n\ngo 1.27\n' > "$tmp/plain/go.mod"
  printf 'package plain\n\nconst Z = 3\n' > "$tmp/plain/plain.go"
  local out="$tmp/out"
  ok_case() { # name, expected exit (0 or 1), message that must be in the output, command...
    local name=$1 want=$2 pat=$3; shift 3
    local rc=0 got=0
    ( cd "$tmp" && "$@" ) > "$out" 2>&1 || rc=$?
    [ "$rc" -ne 0 ] && got=1
    if [ "$got" -ne "$want" ]; then
      echo "self-test: $name: the exit status is $rc, want $([ "$want" -eq 0 ] && echo 0 || echo non-zero)"; sed 's/^/    /' "$out"; fail=true
    fi
    if ! grep -q -- "$pat" "$out"; then
      echo "self-test: $name: the output lacks '$pat'"; sed 's/^/    /' "$out"; fail=true
    fi
  }
  local self; self=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cross-vet.sh
  local all="uses needslib plain"
  # 1. On a system with libc, everything vets.
  ok_case "linux vets all" 0 "vetted 3 modules" env MODULES="$all" "$self" linux
  # 2. On solaris the package that uses the driver's API is left out, the one that only imports it is
  #    vetted for real, and the real driver still does not build there.
  ok_case "solaris leaves out the API user" 0 "left out 1 packages that use the driver's API" env MODULES="$all" "$self" solaris
  ok_case "solaris names it" 0 "left out: example.com/needslib" env MODULES="$all" "$self" solaris
  # 3. A module that vanished from the tree fails loudly.
  ok_case "missing module" 1 "does not exist" env MODULES="uses gone" "$self" linux
  # 4. An empty list fails.
  ok_case "empty list" 1 "MODULES is empty" env MODULES="" "$self" linux
  # 5. No importer found: nothing would be left out, and nothing is learned.
  ok_case "no importer" 1 "listing is broken" env MODULES="plain" "$self" solaris
  # 6. A break in a package that only imports the driver is caught on solaris (the stand-in lets it type-check).
  printf 'package uses\n\nimport _ "%s"\n\nconst Q = undefinedName\n' "$DRIVER" > "$tmp/uses/uses.go"
  ok_case "a break behind the driver" 1 "undefinedName" env MODULES="$all" "$self" solaris
  printf 'package uses\n\nimport _ "%s"\n' "$DRIVER" > "$tmp/uses/uses.go"
  # 7. A break in plain code is caught.
  printf 'package plain\n\nconst Z = undefinedName\n' > "$tmp/plain/plain.go"
  ok_case "a break in plain code" 1 "undefinedName" env MODULES="$all" "$self" solaris
  printf 'package plain\n\nconst Z = 3\n' > "$tmp/plain/plain.go"
  # 8. A package left out for another reason than the driver is a break, not a limit.
  printf 'package plain\n\nimport _ "example.com/nope"\n' > "$tmp/plain/plain.go"
  ok_case "left out for another reason" 1 "does not import $LIBC" env MODULES="$all" "$self" solaris
  printf 'package plain\n\nconst Z = 3\n' > "$tmp/plain/plain.go"
  # 9. libc supports the system after all: the stand-in is stale.
  printf 'package libc\n\nconst X = 1\n' > "$tmp/libc/libc.go"
  ok_case "stale stand-in" 1 "no longer needed" env MODULES="$all" "$self" solaris
  $fail && return 1
  echo "self-test: ok"
}

case "${1:-}" in
  --self-test) self_test ;;
  "") echo "usage: cross-vet.sh GOOS | --self-test" >&2; exit 2 ;;
  *) vet_system "$1" ;;
esac
