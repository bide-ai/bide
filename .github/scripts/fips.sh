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
#   fips.sh check-excluded      under GOFIPS140=v1.0.0, run exactly the excluded tests (go test -json,
#                               needs jq) and fail unless each entry fails and its own output carries the
#                               ML-DSA "unavailable" error, and nothing panics (a listed test that passes,
#                               or no longer exists, is a stale entry). A parent test that fails in its
#                               setup never reaches its ed25519 and ecdsa-p256 subtests under v1.0.0;
#                               those run in the off and v1.26.0 configurations.
#
# The mode is not a validation claim: bide makes none.
set -euo pipefail
set -f # patterns are regular expressions, never globs

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
list=${FIPS_EXCLUDED:-$root/.github/fips-v1.0.0-excluded.txt}

# Tests that must pass in every configuration: Ed25519 and ECDSA P-256 signing and verification, the
# anchored store and approvals under P-256, the mode check itself, and one live round trip each against
# Redis (the script digest, after SCRIPT FLUSH) and Postgres (REDIS_ADDR and PG_DSN set), so a run
# without those services fails instead of skipping.
REQUIRED=${REQUIRED:-"TestRunsInTheModeTheJobNames TestApprovedSchemesSignAndVerifyUnderFIPSOnly TestMLDSAAndHybridUnderTheModuleVersion TestKeygenSchemesUnderTheFIPSModuleVersion TestAppendAfterScriptFlush TestWithoutMigration_OpensWhatOpenMigrated TestP256_AuditedStoreAnchorsSTH TestP256_ApprovalsAndEvidenceEndToEnd TestSign_Roundtrip"}

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
  command -v jq >/dev/null || { err "check-excluded needs jq"; return 1; }
  local m p names names_re json errf n outs fails fail=0 count
  export GODEBUG=fips140=only GOFIPS140=v1.0.0
  cd "$root"
  for m in $(entries | awk '{ print $1 }' | LC_ALL=C sort -u); do
    for p in $(entries | awk -v m="$m" '$1 == m { print $2 }' | LC_ALL=C sort -u); do
      names=$(pkg_entries "$m" "$p")
      names_re=$(awk -F/ '{ print $1 }' <<< "$names" | LC_ALL=C sort -u | while read -r t; do quote "$t"; done | join_alt)
      # stdout only: go test -json writes the JSON stream there, while "go: downloading ..." and other
      # messages go to stderr (kept for the error message), so a cold module cache cannot corrupt it.
      errf=$(mktemp)
      json=$(cd "$m" && go test -count=1 -json -run "^$names_re\$" "$p" 2> "$errf" | grep '^{' || true)
      if ! jq -e -s 'length > 0' <<< "$json" > /dev/null 2>&1; then
        err "$m $p: go test -json under GOFIPS140=v1.0.0 produced no readable JSON (not a stale entry; the check could not run): $(head -c 400 "$errf")"
        fail=1; rm -f "$errf"; continue
      fi
      rm -f "$errf"
      # No test of the package may have panicked: an ML-DSA refusal is an error, never a panic.
      if jq -r 'select(.Action=="output") | .Output' <<< "$json" 2>/dev/null | grep -q '^panic: '; then
        err "$m $p: a panic under GOFIPS140=v1.0.0 (a refusal must be an error)"; fail=1
      fi
      for n in $names; do
        # Each entry on its own: it must fail (itself or through a subtest) and its own output must
        # carry the ML-DSA "unavailable" error, so another cause of failure cannot hide behind a stale entry.
        fails=$(jq -r --arg n "$n" 'select(.Action=="fail" and .Test != null and (.Test==$n or (.Test|startswith($n+"/")))) | .Test' <<< "$json")
        outs=$(jq -r --arg n "$n" 'select(.Action=="output" and .Test != null and (.Test==$n or (.Test|startswith($n+"/")))) | .Output' <<< "$json")
        if [ -z "$fails" ]; then
          err "$m $p $n: listed as excluded but does not fail under GOFIPS140=v1.0.0 (stale entry)"; fail=1
        elif ! grep -q "unavailable in FIPS 140-3 Go Cryptographic Module" <<< "$outs"; then
          err "$m $p $n: fails under GOFIPS140=v1.0.0, but not with the ML-DSA unavailable error"; fail=1
        fi
      done
      count=$(wc -l <<< "$names" | tr -d ' ')
      echo "$m $p: $count entries checked one by one"
    done
  done
  return $fail
}

case "${1:-}" in
  run) run "${2:?usage: fips.sh run off|v1.0.0|v1.26.0}" ;;
  check-excluded) check_excluded ;;
  *) sed -n '2,19p' "$0"; exit 2 ;;
esac
