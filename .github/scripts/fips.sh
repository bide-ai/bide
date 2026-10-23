#!/usr/bin/env bash
# fips.sh: run the test suite under Go's FIPS 140-3 mode, as the "Test (FIPS 140-3)" CI job does.
#
#   fips.sh run VERSION         go build and go test -count=1 -v every module of $MODULES with
#                               GODEBUG=fips140=only and GOFIPS140=VERSION ("off" is the in-tree module;
#                               v1.0.0 and v1.26.0 are frozen modules). For v1.0.0 the tests listed in
#                               .github/fips-v1.0.0-excluded.txt (they need ML-DSA, which that module
#                               lacks) are skipped, nothing else. Then every test in $REQUIRED must
#                               have passed (a PASS line), so a run that did not exercise them fails.
#                               BIDE_EXPECT_FIPS=VERSION is set, so internal/fipsmode proves the
#                               binary runs in the mode that was asked for.
#   fips.sh check-excluded      under GOFIPS140=v1.0.0, run exactly the excluded tests and fail unless
#                               each one fails, and the ML-DSA "unavailable" error appears (a listed test
#                               that passes, or no longer exists, is a stale entry).
#
# The mode is not a validation claim: bide makes none.
set -euo pipefail
set -f # patterns are regular expressions, never globs

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
list=${FIPS_EXCLUDED:-$root/.github/fips-v1.0.0-excluded.txt}

# Tests that must pass in every configuration: Ed25519 and ECDSA P-256 signing and verification, the
# anchored store and approvals under P-256, and the mode check itself.
REQUIRED=${REQUIRED:-"TestRunsInTheModeTheJobNames TestApprovedSchemesSignAndVerifyUnderFIPSOnly TestMLDSAAndHybridUnderTheModuleVersion TestKeygenSchemesUnderTheFIPSModuleVersion TestP256_AuditedStoreAnchorsSTH TestP256_ApprovalsAndEvidenceEndToEnd TestSign_Roundtrip"}

err() { echo "::error::$*"; }

# entries: the non-comment lines of the exclusion list.
entries() { grep -vE '^[[:space:]]*(#|$)' "$list"; }

# quote S: S as a regular expression that matches it literally.
quote() { sed -E 's/[][\\.*^$+?(){}|]/\\&/g' <<< "$1"; }

# join_alt: the lines on stdin as (a|b|c).
join_alt() { paste -sd'|' - | sed 's/^/(/; s/$/)/'; }

# pkg_entries MODULE PACKAGE: the TEST[/SUBTEST] of every entry of that package.
pkg_entries() { entries | awk -v m="$1" -v p="$2" '$1 == m && $2 == p { print $3 }'; }

# skip_args MODULE PACKAGE: print the go test flags for a package with entries, one invocation per line:
# first the whole-test skip (top-level entries and the parents of subtest entries), then, per parent
# with subtest entries, a run of that parent with its listed subtests skipped.
invocations() {
  local m=$1 p=$2 tops parents par subs
  tops=$(pkg_entries "$m" "$p" | awk -F/ '{ print $1 }' | LC_ALL=C sort -u)
  [ -n "$tops" ] || return 0
  echo "-skip ^$(while read -r t; do quote "$t"; done <<< "$tops" | join_alt)\$"
  parents=$(pkg_entries "$m" "$p" | grep / | awk -F/ '{ print $1 }' | LC_ALL=C sort -u || true)
  for par in $parents; do
    subs=$(pkg_entries "$m" "$p" | grep "^$par/" | cut -d/ -f2- | while read -r s; do quote "$s"; done | join_alt)
    echo "-run ^$(quote "$par")\$ -skip ^$(quote "$par")\$/^$subs\$"
  done
}

run() {
  local version=$1 m pkgs excl plain p fail=0 out flags
  out=$(mktemp)
  export GODEBUG=fips140=only BIDE_EXPECT_FIPS=$version GOFIPS140=$version
  cd "$root"
  for m in ${MODULES:?}; do
    echo "== $m (GOFIPS140=$GOFIPS140 GODEBUG=$GODEBUG) =="
    pkgs=$(cd "$m" && go list ./...)
    excl=
    if [ "$version" = v1.0.0 ]; then
      # the import paths of the packages that have entries
      for p in $(entries | awk -v m="$m" '$1 == m { print $2 }' | LC_ALL=C sort -u); do
        excl+=" $(cd "$m" && go list "$p")"
      done
    fi
    plain=$(for p in $pkgs; do listed "$p" $excl || echo "$p"; done)
    (cd "$m" && go build -o /dev/null ./...) || { err "$m: go build failed"; fail=1; continue; }
    if [ -n "$plain" ]; then
      (cd "$m" && go test -count=1 -v $plain) 2>&1 | tee -a "$out" || fail=1
    fi
    for p in $excl; do
      while read -r flags; do
        [ -n "$flags" ] || continue
        echo "-- $p: go test $flags"
        (cd "$m" && go test -count=1 -v $flags "$p") 2>&1 | tee -a "$out" || fail=1
      done < <(invocations "$m" "$(rel_of "$m" "$p")")
    done
  done
  for t in $REQUIRED; do
    grep -qE -- "^--- PASS: $t( |\$)" "$out" || { err "required test $t has no PASS line: the FIPS run did not exercise it"; fail=1; }
  done
  rm -f "$out"
  return $fail
}

# listed WORD LIST...: success when WORD is one of LIST.
listed() { local w=$1 x; shift; for x in "$@"; do [ "$x" != "$w" ] || return 0; done; return 1; }

# rel_of MODULE IMPORTPATH: the package's directory relative to the module, as the exclusion list names it.
rel_of() {
  local m=$1 p=$2 d
  d=$(cd "$m" && go list -f '{{.Dir}}' "$p")
  d=${d#"$(cd "$m" && pwd)"}
  echo ".${d}"
}

check_excluded() {
  local m p names flags out fail=0 n names_re
  export GODEBUG=fips140=only GOFIPS140=v1.0.0
  cd "$root"
  for m in $(entries | awk '{ print $1 }' | LC_ALL=C sort -u); do
    for p in $(entries | awk -v m="$m" '$1 == m { print $2 }' | LC_ALL=C sort -u); do
      names=$(pkg_entries "$m" "$p")
      names_re=$(awk -F/ '{ print $1 }' <<< "$names" | LC_ALL=C sort -u | while read -r t; do quote "$t"; done | join_alt)
      out=$(cd "$m" && go test -count=1 -v -run "^$names_re\$" "$p" 2>&1 || true)
      for n in $names; do
        # a whole test must show its own FAIL line; a subtest entry its own FAIL line or one of its
        # subtests (a subtest name with a slash in it is listed by its first elements)
        if ! grep -qE -- "^\s*--- FAIL: $(quote "$n")( |/|\$)" <<< "$out"; then
          err "$m $p $n: listed as excluded but does not fail under GOFIPS140=v1.0.0 (stale entry, or a different failure)"; fail=1
        fi
      done
      grep -q "unavailable in FIPS 140-3 Go Cryptographic Module" <<< "$out" || { err "$m $p: its excluded tests fail, but not with the ML-DSA unavailable error"; fail=1; }
      echo "$m $p: $(grep -cE '^\s*--- FAIL' <<< "$out" || true) failing tests checked against $(wc -l <<< "$names" | tr -d ' ') entries"
    done
  done
  return $fail
}

case "${1:-}" in
  run) run "${2:?usage: fips.sh run off|v1.0.0|v1.26.0}" ;;
  check-excluded) check_excluded ;;
  *) sed -n '2,19p' "$0"; exit 2 ;;
esac
