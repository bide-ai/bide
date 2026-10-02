#!/usr/bin/env bash
# shards.sh: the shards CI splits its slowest jobs into, the check that every Go module and every
# pull-request TLC configuration is in exactly one of them, and the result of the required check
# that stands for them.
#
# A shard list ($SHARDS) holds one shard per line, "name: entry entry ...". CI's TEST_SHARDS
# (ci.yml) names what each Linux test shard builds and tests: a module ("mcp", "." for the core),
# or one package of a module ("MODULE:DIR", such as ".:agent", the package in the core's agent
# directory and not its subpackages). A module entry covers the module's packages that no package
# entry names. The Models workflow's MODEL_SHARDS (models.yml) names the configuration directories
# under spec/tla each Models shard checks (check.sh's TLC_DIRS: a model's directory, or its
# regress, findings or limits directory).
#
#   shards.sh names                  every shard's name, one per line
#   shards.sh json                   every shard's name, as a JSON array (a job matrix)
#   shards.sh get NAME               shard NAME's entries, space-separated
#   shards.sh plan NAME              what shard NAME's job builds and tests, one line per entry: the
#                                    module's directory, then "./..." for a whole module, or the
#                                    import paths of its packages (go list; needs Go)
#   shards.sh run NAME FLAG...       run shard NAME's plan: in each module's directory, go build the
#                                    packages with non-test Go files (as go build ./... skips a
#                                    test-only package), then go test FLAG... the packages
#   shards.sh check-modules          fail unless every module in $MODULES has a module entry in
#                                    exactly one shard, every package entry is in exactly one shard
#                                    and names a package directory of a module in $MODULES, and
#                                    every module entry is a module in $MODULES (no Go needed)
#   shards.sh check-packages         for each module split by package entries: fail unless every
#                                    package go list finds in it is in exactly one shard's plan, and
#                                    each plan's packages are the module's (needs Go)
#   shards.sh check-models [ROOT]    fail unless every ci, regress, finding and limit configuration
#                                    ROOT/spec/tla/check.sh runs is run by exactly one shard; each
#                                    shard's configurations come from check.sh itself, with the
#                                    shard's entries as TLC_DIRS, as the shard's job runs it
#   shards.sh select ROOT [MODEL...]  the Models shards to run, as a JSON array: every shard, or
#                                    with MODELs (the models a pull request changes) the shards
#                                    holding them; fails unless each MODEL is a model directory with
#                                    a ci configuration (ROOT/spec/tla/check.sh list ci), so a change
#                                    to anything else under spec/tla/<dir>/ never passes unchecked
#   shards.sh verdict PLAN RUN JOB=RESULT...
#                                    the required check's result: fail unless PLAN (the result of
#                                    the job that planned the shards) is success and either RUN is
#                                    "false" (nothing to check) and every JOB was skipped or
#                                    succeeded, or every JOB succeeded; a failed, cancelled or
#                                    skipped shard fails it
#   shards.sh --self-test            prove each check fails on a missing, doubled or unknown entry
#                                    (module, package, configuration directory), and the verdict on
#                                    a failed, cancelled or skipped shard (needs Go)
set -euo pipefail
set -f # entries are paths, never globs

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
self=$here/$(basename "${BASH_SOURCE[0]}")

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

where_test='TEST_SHARDS in .github/workflows/ci.yml'

check_modules() {
  local all mods pkgs listed m e n ok=0
  parse || return 1
  all=$(printf '%s\n' ${entries[@]})
  mods=$(grep -v : <<< "$all" || true)
  pkgs=$(grep : <<< "$all" || true)
  listed=$(printf '%s\n' ${MODULES:-})
  for m in $listed; do
    n=$(count "$m" "$mods")
    [ "$n" = 1 ] || { err "module $m is in $n shards, want exactly one: name it on exactly one shard's line of $where_test"; ok=1; }
  done
  for m in $(LC_ALL=C sort -u <<< "$mods"); do
    [ "$(count "$m" "$listed")" -gt 0 ] || { err "shard entry $m is not a module in MODULES: remove it from $where_test"; ok=1; }
  done
  for e in $(LC_ALL=C sort -u <<< "$pkgs"); do
    n=$(count "$e" "$pkgs")
    [ "$n" = 1 ] || { err "package entry $e is in $n shards, want exactly one: keep it on one shard's line of $where_test"; ok=1; }
    [ "$(count "${e%%:*}" "$listed")" -gt 0 ] || { err "package entry $e: ${e%%:*} is not a module in MODULES"; ok=1; }
    if ! [[ ${e#*:} =~ ^[A-Za-z0-9_][A-Za-z0-9_./-]*$ ]] || [[ ${e#*:} == *...* || ${e#*:} == */ || ${e#*:} == *//* || ${e#*:} == *./* ]]; then
      err "package entry $e: want MODULE:DIR, one package directory relative to the module (no ./ and no ...)"; ok=1
    fi
  done
  [ $ok = 0 ] && echo "every module is in exactly one shard, and every package entry in one (${#names[@]} shards)"
  return $ok
}

# plan NAME: see the header. A module entry's packages are the module's (go list ./...) but those
# its package entries, in any shard, name; with no package entry, "./...", as before the split.
plan() {
  local name=$1 list= e x ex all drop keep i
  parse || return 1
  for i in "${!names[@]}"; do [ "${names[$i]}" != "$name" ] || list=${entries[$i]}; done
  [ -n "$list" ] || { err "no shard $name"; return 1; }
  for e in $list; do
    case "$e" in
      *:*)
        x=$(cd "${e%%:*}" && go list "./${e#*:}") || { err "$e: go list failed"; return 1; }
        echo "${e%%:*} $x" ;;
      *)
        ex=
        for x in ${entries[@]}; do case "$x" in "$e":*) ex+=" ${x#*:}" ;; esac; done
        if [ -z "$ex" ]; then echo "$e ./..."; continue; fi
        all=$(cd "$e" && go list ./...) || { err "$e: go list failed"; return 1; }
        drop=
        for x in $ex; do
          drop+=$(cd "$e" && go list "./$x") || { err "$e:$x: go list failed"; return 1; }
          drop+=$'\n'
        done
        keep=$(grep -vxF -f <(printf '%s' "$drop") <<< "$all" || true)
        [ -n "$keep" ] || { err "shard $name: module $e keeps no package once its package entries are taken out"; return 1; }
        echo "$e" $keep ;;
    esac
  done
}

run_shard() {
  local name=$1 plan m pkgs build
  shift
  plan=$(plan "$name") || { echo "$plan"; return 1; }
  echo "$plan"
  while read -r m pkgs; do
    echo "== $m =="
    build=$(cd "$m" && go list -f '{{if .GoFiles}}{{.ImportPath}}{{end}}' $pkgs) || return 1
    if [ -n "$build" ]; then (cd "$m" && go build $build) || return 1; fi
    (cd "$m" && go test "$@" $pkgs) || return 1
  done <<< "$plan"
}

check_packages() {
  local m i n p want got out ok=0
  parse || return 1
  for m in $(printf '%s\n' ${entries[@]} | grep : | cut -d: -f1 | LC_ALL=C sort -u); do
    want=$(cd "$m" && go list ./...) || { err "$m: go list failed"; return 1; }
    got=
    for i in "${!names[@]}"; do
      out=$(plan "${names[$i]}") || { err "shard ${names[$i]}: $out"; ok=1; continue; }
      got+=$(awk -v m="$m" '$1 == m { for (i = 2; i <= NF; i++) print $i }' <<< "$out")$'\n'
    done
    for p in $want; do
      n=$(count "$p" "$got")
      [ "$n" = 1 ] || { err "package $p (module $m) is in $n shards' plans, want exactly one: check the $m entries of $where_test"; ok=1; }
    done
    for p in $(grep -v '^$' <<< "$got" | LC_ALL=C sort -u); do
      [ "$(count "$p" "$want")" -gt 0 ] || { err "shard package $p is not a package of module $m"; ok=1; }
    done
    [ $ok = 0 ] && echo "module $m: every package ($(wc -l <<< "$want" | tr -d ' ')) is in exactly one shard"
  done
  return $ok
}

where_models='MODEL_SHARDS in .github/workflows/models.yml'

# fix_dir DIR N: what to change when the configs in DIR run in N shards (0, or more than 1).
fix_dir() {
  local i e
  if [ "$2" != 0 ]; then echo "keep \"$1\" on only one shard's line of $where_models"; return; fi
  for i in "${!names[@]}"; do
    for e in ${entries[$i]}; do
      if [ "${e%%/*}" = "${1%%/*}" ]; then
        echo "add \"$1\" to the \"${names[$i]}:\" line of $where_models (the shard holding ${1%%/*}), or to another shard's line"
        return
      fi
    done
  done
  echo "add \"$1\" to one shard's line of $where_models (a new model: add it to the shard with the least work, or add a shard)"
}

check_models() {
  local check=${1:-.}/spec/tla/check.sh all got out bad c d k n i ok=0
  parse || return 1
  all=$(env -u TLC_MODELS -u TLC_DIRS "$check" list ci regress finding limit) || { err "check.sh list failed"; return 1; }
  [ -n "$all" ] || { err "check.sh lists no configuration"; return 1; }
  got=
  for i in "${!names[@]}"; do
    if ! out=$(env -u TLC_MODELS TLC_DIRS="${entries[$i]}" "$check" list ci regress finding limit 2>&1); then
      err "shard ${names[$i]}: $out (remove it from the \"${names[$i]}:\" line of $where_models)"; ok=1; continue
    fi
    [ -n "$out" ] || { err "shard ${names[$i]} runs no configuration: remove its line from $where_models, or give it a directory"; ok=1; continue; }
    got+=$out$'\n'
  done
  # One error per directory and shard count, naming the configs' directory and the fix.
  bad=$(for c in $all; do n=$(count "$c" "$got"); [ "$n" = 1 ] || echo "$(dirname "$c") $n"; done | LC_ALL=C sort | uniq -c)
  while read -r k d n; do
    [ -n "$d" ] || continue
    err "$k configuration(s) in spec/tla/$d run in $n shards, want exactly one: $(fix_dir "$d" "$n")"; ok=1
  done <<< "$bad"
  for c in $(LC_ALL=C sort -u <<< "$got"); do
    [ "$(count "$c" "$all")" -gt 0 ] || { err "shard config $c is not in the pull-request set"; ok=1; }
  done
  [ $ok = 0 ] && echo "every configuration ($(wc -l <<< "$all" | tr -d ' ')) is run by exactly one shard (${#names[@]} shards)"
  return $ok
}

select_shards() {
  local root=$1 m e i out sel=()
  shift
  parse || return 1
  for m in "$@"; do
    out=$(env -u TLC_DIRS TLC_MODELS="$m" "$root/spec/tla/check.sh" list ci 2>&1) || { err "spec/tla/$m changed: $out"; return 1; }
    [ -n "$out" ] || { err "spec/tla/$m changed, and it has no ci configuration: a model needs one on every pull request"; return 1; }
  done
  for i in "${!names[@]}"; do
    for e in ${entries[$i]}; do
      if [ $# = 0 ] || [[ " $* " == *" ${e%%/*} "* ]]; then sel+=("${names[$i]}"); break; fi
    done
  done
  for m in "$@"; do
    [[ " $(printf '%s\n' ${entries[@]} | cut -d/ -f1 | tr '\n' ' ') " == *" $m "* ]] || { err "spec/tla/$m changed, and no shard holds it: $(fix_dir "$m" 0)"; return 1; }
  done
  printf '%s\n' "${sel[@]}" | jq -R . | jq -cs .
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
  expect pass "a package entry" m $'x: .\ny: a b/c .:p'
  expect fail "package entry .:p in two shards" m $'x: . .:p\ny: a b/c .:p'
  expect fail "package entry of a module not in MODULES" m $'x: .\ny: a b/c d:p'
  expect fail "package entry .:./p" m $'x: .\ny: a b/c .:./p'
  expect fail "package entry .:p/..." m $'x: .\ny: a b/c .:p/...'
  expect fail "a package entry and no module entry" m $'x: .:p\ny: a b/c'
  # Packages, against go list in a scratch module (the core) with packages p, p/sub and q, and a
  # module m that no package entry splits.
  local g=$tmp/go
  mkdir -p "$g/p/sub" "$g/q" "$g/m"
  printf 'module example.com/r\n\ngo 1.21\n' > "$g/go.mod"
  printf 'module example.com/m\n\ngo 1.21\n' > "$g/m/go.mod"
  for d in p p/sub q m; do printf 'package %s\n' "$(basename "$d")" > "$g/$d/x.go"; done
  mkdir -p "$g/t"; printf 'package t\n\nimport "testing"\n\nfunc TestT(t *testing.T) {}\n' > "$g/t/x_test.go"
  pk() { (cd "$g" && GOWORK=off GOFLAGS= GOTOOLCHAIN=local MODULES=". m" SHARDS=$1 "$self" check-packages); }
  expect pass "p alone, the rest of the core" pk $'x: .:p\ny: .\nz: m'
  expect pass "p and q alone, the rest of the core" pk $'x: .:p .:q\ny: .\nz: m'
  expect fail "package p in two shards" pk $'x: .:p\ny: . .:p\nz: m'
  expect fail "no such package .:nope" pk $'x: .:nope\ny: .\nz: m'
  expect fail "the core's module entry keeps no package" pk $'x: .:p .:p/sub .:q .:t\ny: .\nz: m'
  expect fail "the core in two module entries" pk $'x: .:p .\ny: .\nz: m'
  pl() { (cd "$g" && GOWORK=off GOFLAGS= GOTOOLCHAIN=local SHARDS=$'x: .:p\ny: . m' "$self" plan "$1") > "$tmp/plan" 2>&1 && [ "$(cat "$tmp/plan")" = "$2" ]; }
  expect pass "the plan of a package entry" pl x ". example.com/r/p"
  expect pass "the plan of the rest of the core" pl y $'. example.com/r/p/sub example.com/r/q example.com/r/t\nm ./...'
  # A shard's run builds only the packages with non-test Go files (t holds only a test), and
  # tests them all.
  rn() { (cd "$g" && GOWORK=off GOFLAGS= GOTOOLCHAIN=local SHARDS=$'x: .:p\ny: . m' "$self" run "$1" -count=1) > "$tmp/run" 2>&1 && grep -q "$2" "$tmp/run"; }
  expect pass "a shard with a test-only package" rn y '^ok .*example.com/r/t'
  expect pass "a shard of one package" rn x '^?.*example.com/r/p'
  expect fail "a shard whose test fails" rn y 'never printed'
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
  # The shards to run for the changed models.
  mkdir -p "$t/n"; touch "$t/n/NMC.tla"; cfg n/deep.cfg nightly
  sl() { # sl SHARDS WANT MODEL...: select prints WANT
    local shards=$1 want=$2
    shift 2
    SHARDS=$shards "$self" select "$tmp/repo" "$@" > "$tmp/sel" && [ "$(tail -1 "$tmp/sel")" = "$want" ]
  }
  local two=$'s1: a a/regress c\ns2: b b/limits'
  expect pass "every shard with no model named" sl "$two" '["s1","s2"]'
  expect pass "the shard holding b" sl "$two" '["s2"]' b
  expect pass "the shards holding c and b" sl "$two" '["s1","s2"]' c b
  expect fail "a changed directory that is no model" sl "$two" '[]' notamodel
  expect fail "a changed model with only nightly configurations" sl $'s1: a a/regress c n\ns2: b b/limits' '["s1"]' n
  expect fail "a changed model no shard holds" sl $'s1: a a/regress\ns2: b b/limits' '["s2"]' b c
  expect fail "a malformed shard list" sl $'s1 a a/regress c\ns2: b b/limits' '["s2"]' b
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
  plan) plan "${2:-}" ;;
  run) shift; [ $# -ge 1 ] || { err "run: NAME FLAG..."; exit 1; }; run_shard "$@" ;;
  check-packages) check_packages ;;
  get)
    parse
    for i in "${!names[@]}"; do [ "${names[$i]}" != "${2:-}" ] || { echo "${entries[$i]}"; exit 0; }; done
    err "no shard ${2:-}"; exit 1 ;;
  check-modules) check_modules ;;
  check-models) check_models "${2:-.}" ;;
  select) shift; [ $# -ge 1 ] || { err "select: ROOT [MODEL...]"; exit 1; }; select_shards "$@" ;;
  verdict) shift; [ $# -ge 2 ] || { err "verdict: PLAN RUN JOB=RESULT..."; exit 1; }; verdict "$@" ;;
  --self-test) self_test ;;
  *) echo "usage: see the header of $0" >&2; exit 2 ;;
esac
