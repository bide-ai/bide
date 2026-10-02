#!/usr/bin/env bash
# shards.sh: the shards CI splits its slowest jobs into, the check that every Go module and every
# pull-request TLC configuration is in exactly one of them, and the result of the required check
# that stands for them.
#
# A shard list ($SHARDS) holds one shard per line, "name: entry entry ...". CI's TEST_SHARDS
# (ci.yml) names the Go modules each Linux test shard builds and tests; the Models workflow's
# MODEL_SHARDS (models.yml) names the configuration directories under spec/tla each Models shard
# checks (check.sh's TLC_DIRS: a model's directory, or its regress, findings or limits directory).
#
#   shards.sh names                  every shard's name, one per line
#   shards.sh json                   every shard's name, as a JSON array (a job matrix)
#   shards.sh get NAME               shard NAME's entries, space-separated
#   shards.sh check-modules          fail unless every module in $MODULES is in exactly one shard
#                                    and every entry is a module in $MODULES
#   shards.sh check-models [ROOT]    fail unless every ci, regress, finding and limit configuration
#                                    ROOT/spec/tla/check.sh runs is run by exactly one shard; each
#                                    shard's configurations come from check.sh itself, with the
#                                    shard's entries as TLC_DIRS, as the shard's job runs it
#   shards.sh verdict PLAN RUN JOB=RESULT...
#                                    the required check's result: fail unless PLAN (the result of
#                                    the job that planned the shards) is success and either RUN is
#                                    "false" (nothing to check) and every JOB was skipped or
#                                    succeeded, or every JOB succeeded; a failed, cancelled or
#                                    skipped shard fails it
#   shards.sh --self-test            prove each check fails on a missing, doubled or unknown entry,
#                                    and the verdict on a failed, cancelled or skipped shard
set -euo pipefail
set -f # entries are paths, never globs

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

err() { echo "::error::$*"; }

# parse: read $SHARDS into names[] and entries[]; fail on a malformed line, a doubled name or a
# shard with no entries. Blank lines and "#" comments are ignored.
parse() {
  local line name list i ok=0
  names=(); entries=()
  while IFS= read -r line; do
    line=${line%%#*}
    [ -n "${line//[[:space:]]/}" ] || continue
    if ! [[ $line =~ ^[[:space:]]*([A-Za-z0-9._-]+):(.*)$ ]]; then
      err "shard list: malformed line \"$line\" (want \"name: entry...\")"; ok=1; continue
    fi
    name=${BASH_REMATCH[1]}
    list=$(echo ${BASH_REMATCH[2]})
    for i in "${!names[@]}"; do
      [ "${names[$i]}" != "$name" ] || { err "shard list: shard $name is listed twice"; ok=1; }
    done
    [ -n "$list" ] || { err "shard list: shard $name has no entries"; ok=1; }
    names+=("$name"); entries+=("$list")
  done <<< "${SHARDS:-}"
  [ ${#names[@]} -gt 0 ] || { err "shard list: no shards"; ok=1; }
  return $ok
}

# count WORD LIST: how many lines of LIST are exactly WORD.
count() { grep -cxF -- "$1" <<< "$2" || true; }

check_modules() {
  local all m n ok=0
  parse || return 1
  all=$(printf '%s\n' ${entries[@]})
  for m in ${MODULES:-}; do
    n=$(count "$m" "$all")
    [ "$n" = 1 ] || { err "module $m is in $n shards, want exactly one"; ok=1; }
  done
  for m in $(LC_ALL=C sort -u <<< "$all"); do
    [ "$(count "$m" "$(printf '%s\n' ${MODULES:-})")" -gt 0 ] || { err "shard entry $m is not a module in MODULES"; ok=1; }
  done
  [ $ok = 0 ] && echo "every module is in exactly one shard (${#names[@]} shards)"
  return $ok
}

check_models() {
  local check=${1:-.}/spec/tla/check.sh all got out c n i ok=0
  parse || return 1
  all=$(env -u TLC_MODELS -u TLC_DIRS "$check" list ci regress finding limit) || { err "check.sh list failed"; return 1; }
  [ -n "$all" ] || { err "check.sh lists no configuration"; return 1; }
  got=
  for i in "${!names[@]}"; do
    if ! out=$(env -u TLC_MODELS TLC_DIRS="${entries[$i]}" "$check" list ci regress finding limit 2>&1); then
      err "shard ${names[$i]}: $out"; ok=1; continue
    fi
    [ -n "$out" ] || { err "shard ${names[$i]} runs no configuration"; ok=1; continue; }
    got+=$out$'\n'
  done
  for c in $all; do
    n=$(count "$c" "$got")
    [ "$n" = 1 ] || { err "$c is run by $n shards, want exactly one (assign its directory to one shard in MODEL_SHARDS)"; ok=1; }
  done
  for c in $(LC_ALL=C sort -u <<< "$got"); do
    [ "$(count "$c" "$all")" -gt 0 ] || { err "shard config $c is not in the pull-request set"; ok=1; }
  done
  [ $ok = 0 ] && echo "every configuration ($(wc -l <<< "$all" | tr -d ' ')) is run by exactly one shard (${#names[@]} shards)"
  return $ok
}

verdict() {
  local plan=$1 run=$2 j ok=0
  shift 2
  echo "planning job: $plan; shards run: ${run:-true}"
  for j in "$@"; do echo "  ${j%%=*}: ${j#*=}"; done
  [ "$plan" = success ] || { err "the planning job did not succeed ($plan)"; return 1; }
  for j in "$@"; do
    case "$run:${j#*=}" in
      *:success) ;;
      false:skipped) ;;
      *) err "${j%%=*}: ${j#*=}"; ok=1 ;;
    esac
  done
  return $ok
}

self_test() {
  local fail=false
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  expect() { # expect pass|fail DESCRIPTION COMMAND...: run COMMAND and check its outcome
    local want=$1 what=$2 got=pass
    shift 2
    "$@" > "$tmp/out" 2>&1 || got=fail
    if [ "$got" != "$want" ]; then
      echo "self-test: $what: got $got, want $want"; cat "$tmp/out"; fail=true
    fi
  }
  # Modules.
  m() { MODULES=". a b/c" SHARDS=$1 "$0" check-modules; }
  expect pass "every module in one shard" m $'x: .\ny: a b/c # comment\n'
  expect fail "module b/c in no shard" m $'x: .\ny: a'
  expect fail "module a in two shards" m $'x: . a\ny: a b/c'
  expect fail "entry d not a module" m $'x: .\ny: a b/c d'
  expect fail "shard x named twice" m $'x: .\nx: a b/c'
  expect fail "shard z with no entries" m $'x: .\ny: a b/c\nz:'
  expect fail "malformed line" m $'x: .\ny a b/c'
  # Models, against the real check.sh in a scratch tree.
  local t=$tmp/repo/spec/tla
  mkdir -p "$t/a/regress" "$t/a/limits" "$t/b/limits" "$t/notamodel"
  cp "$here/../../spec/tla/check.sh" "$t/"
  cfg() { printf '\\* GROUP: %s\n\\* EXPECT: pass\n' "$2" > "$t/$1"; }
  touch "$t/a/AMC.tla" "$t/b/BMC.tla"
  cfg a/one.cfg ci; cfg a/deep.cfg nightly; cfg a/regress/r.cfg regress
  cfg b/two.cfg ci; cfg b/limits/l.cfg limit; cfg notamodel/x.cfg ci
  mt() { SHARDS=$1 "$0" check-models "$tmp/repo"; }
  expect pass "every config in one shard" mt $'s1: a a/regress\ns2: b b/limits'
  expect fail "a/regress in no shard" mt $'s1: a\ns2: b b/limits'
  expect fail "a/regress in two shards" mt $'s1: a a/regress\ns2: b b/limits a/regress'
  expect fail "no such directory a/findings" mt $'s1: a a/regress a/findings\ns2: b b/limits'
  expect fail "notamodel is not a model" mt $'s1: a a/regress\ns2: b b/limits notamodel'
  expect fail "a shard running nothing" mt $'s1: a a/regress\ns2: b b/limits\ns3: a/limits'
  mkdir -p "$t/c"; touch "$t/c/CMC.tla"; cfg c/three.cfg ci
  expect fail "new model c in no shard" mt $'s1: a a/regress\ns2: b b/limits'
  expect pass "new model c assigned" mt $'s1: a a/regress c\ns2: b b/limits'
  # The verdict.
  expect pass "every shard succeeded" "$0" verdict success true x=success y=success
  expect pass "every shard succeeded (run unset)" "$0" verdict success "" x=success
  expect fail "a shard failed" "$0" verdict success true x=success y=failure
  expect fail "a shard cancelled" "$0" verdict success true x=success y=cancelled
  expect fail "a shard skipped" "$0" verdict success true x=success y=skipped
  expect fail "a shard skipped (run unset)" "$0" verdict success "" x=skipped
  expect fail "the planning job failed" "$0" verdict failure true x=skipped
  expect fail "the planning job cancelled" "$0" verdict cancelled false x=skipped
  expect pass "nothing to check, shards skipped" "$0" verdict success false x=skipped y=skipped
  expect fail "nothing to check, a shard failed" "$0" verdict success false x=failure
  $fail && return 1
  echo "self-test: ok"
}

case "${1:-}" in
  names) parse; printf '%s\n' "${names[@]}" ;;
  json) parse; printf '%s\n' "${names[@]}" | jq -R . | jq -cs . ;;
  get)
    parse
    for i in "${!names[@]}"; do [ "${names[$i]}" != "${2:-}" ] || { echo "${entries[$i]}"; exit 0; }; done
    err "no shard ${2:-}"; exit 1 ;;
  check-modules) check_modules ;;
  check-models) check_models "${2:-.}" ;;
  verdict) shift; [ $# -ge 2 ] || { err "verdict: PLAN RUN JOB=RESULT..."; exit 1; }; verdict "$@" ;;
  --self-test) self_test ;;
  *) echo "usage: see the header of $0" >&2; exit 2 ;;
esac
