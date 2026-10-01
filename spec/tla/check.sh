#!/usr/bin/env bash
# check.sh: fetch the pinned TLA+ tools, check that each committed PlusCal translation is up to
# date, and run TLC over the model configurations. See spec/tla/README.md.
#
#   spec/tla/check.sh                 translation check, then every "ci", "regress", "finding"
#                                     and "limit" config (what CI runs on a pull request)
#   spec/tla/check.sh ci|nightly|regress|finding|limit
#                                     the configs of one group
#   spec/tla/check.sh run FILE.cfg... the named configs, whatever their group
#   spec/tla/check.sh translation     fail if a committed translation is stale
#   spec/tla/check.sh translate       re-translate every spec in place
#   spec/tla/check.sh fetch           download and verify the tools only
#   spec/tla/check.sh self-test       prove the translation and checksum checks can fail
#
# Each .cfg names its group and its expected result in comment lines:
#   \* GROUP: ci | nightly | regress | finding | limit
#   \* EXPECT: pass | invariant <Name> | liveness
#   \* VACUITY: skip <reason>   (optional: a passing config in which the effect must never fire)
# "pass" configs are also run once with the vacuity invariant EffectNotReachable, which TLC must
# report violated: a model in which the effect never fires satisfies every safety property.
#
# Needs Java 11 or later (JAVA_HOME or java on PATH), curl, and sha256sum or shasum.
# Environment: BIDE_TLA_CACHE (tool cache; default ~/.cache/bide-tla), TLC_WORKERS (default auto),
# TLC_JOBS (default 1: how many configs run at a time; above 1, each runs with one TLC worker),
# TLC_MODELS (default every model: the model directories whose configs a group runs, such as
# "claims toolcall"), TLC_JAVA_OPTS (extra JVM options).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cache=${BIDE_TLA_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/bide-tla}
workers=${TLC_WORKERS:-auto}
java=${JAVA_HOME:+$JAVA_HOME/bin/}java
summary=()
failures=0

# Every temporary file and directory (TLC metadirs, outputs, translation copies) lives under one
# work directory, removed on exit, failure and interrupt, so a killed run leaves no TLC state
# behind. A vacuity config is written beside its config (see run_cfg) and removed the same way.
work=$(mktemp -d "${TMPDIR:-/tmp}/bide-tla.XXXXXX")
cleanup() {
  if [ -n "${tlc_pid:-}" ]; then kill "$tlc_pid" 2>/dev/null || true; wait "$tlc_pid" 2>/dev/null || true; fi
  rm -rf "$work"
  rm -f "$here"/*/.vacuity-$$.cfg "$here"/*/*/.vacuity-$$.cfg
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
tmpdir() { mktemp -d "$work/d.XXXXXX"; }

die() { echo "check.sh: $*" >&2; exit 1; }

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# verify_jar FILE SHA: fail unless FILE's SHA-256 is SHA.
verify_jar() {
  local got
  got=$(sha256 "$1")
  [ "$got" = "$2" ] || die "$1: SHA-256 $got, want $2 (tools.lock); refusing to run it"
}

# fetch: download each tool in tools.lock into the cache, verify it, and set $jar.
fetch() {
  mkdir -p "$cache"
  local name version sum url file
  while read -r name version sum url; do
    case "$name" in ''|'#'*) continue ;; esac
    file="$cache/$name-$version.jar"
    if [ ! -f "$file" ]; then
      echo "fetching $name $version"
      curl -fsSL --retry 3 -o "$file.part" "$url"
      mv "$file.part" "$file"
    fi
    verify_jar "$file" "$sum"
    [ "$name" = tla2tools ] && jar=$file
  done < "$here/tools.lock"
  [ -n "${jar:-}" ] || die "tools.lock names no tla2tools"
  "$java" -version >/dev/null 2>&1 || die "no java (set JAVA_HOME or put java on PATH)"
}

# specs: every module holding a PlusCal algorithm.
specs() { grep -l -- '--algorithm' "$here"/*/*.tla; }

translate_to() { # translate_to SPEC DIR: translate a copy of SPEC in DIR
  cp "$1" "$2/"
  (cd "$2" && "$java" -cp "$jar" pcal.trans -nocfg "$(basename "$1")" >translate.log 2>&1) ||
    { cat "$2/translate.log" >&2; die "PlusCal translation of $1 failed"; }
}

translation() {
  local s tmp ok=0
  for s in $(specs); do
    tmp=$(tmpdir)
    translate_to "$s" "$tmp"
    if ! diff -u "$s" "$tmp/$(basename "$s")" >"$tmp/diff"; then
      echo "STALE translation: $s (run spec/tla/check.sh translate and commit the result)" >&2
      head -40 "$tmp/diff" >&2
      ok=1
    else
      echo "translation up to date: ${s#"$here"/}"
    fi
    rm -rf "$tmp"
  done
  return $ok
}

translate() {
  local s tmp
  for s in $(specs); do
    tmp=$(tmpdir)
    translate_to "$s" "$tmp"
    cp "$tmp/$(basename "$s")" "$s"
    rm -rf "$tmp"
    echo "translated ${s#"$here"/}"
  done
}

meta() { sed -n "s/^\\\\\\* $1: *//p" "$2" | head -1; }

# mc_of DIR: the model's MC module in DIR (the one *MC.tla file), or nothing.
mc_of() { (cd "$1" && ls ./*MC.tla 2>/dev/null | head -1 | sed 's|^\./||'); }

# tlc CFG OUT DIR: run TLC on CFG (a copy may live elsewhere) against the MC module in DIR; set
# tlc_status to TLC's exit status. TLC runs in the background and is waited for, so an interrupt
# reaches the trap at once, which stops it (tlc_pid) before removing its metadir.
tlc_pid=
tlc() {
  local cfg=$1 out=$2 dir=$3 meta_dir
  meta_dir=$(tmpdir)
  (cd "$dir" && exec "$java" -XX:+UseParallelGC ${TLC_JAVA_OPTS:-} -cp "$jar" tlc2.TLC \
      -workers "$workers" -metadir "$meta_dir" -config "$cfg" "$(mc_of "$dir")") >"$out" 2>&1 &
  tlc_pid=$!
  tlc_status=0
  wait "$tlc_pid" || tlc_status=$?
  tlc_pid=
  rm -rf "$meta_dir"
}

stats() { # stats OUT: "states / depth / time" from TLC's output
  local states depth time
  states=$(sed -n 's/.* \([0-9,]*\) distinct states found.*/\1/p' "$1" | tail -1)
  depth=$(sed -n 's/.*depth of the complete state graph search is \([0-9]*\).*/\1/p' "$1" | tail -1)
  [ -n "$depth" ] || depth=$(grep -c '^State [0-9]*:' "$1" || true)
  time=$(sed -n 's/^Finished in \([^ ]*\) .*/\1/p' "$1" | tail -1)
  echo "${states:-?} states, depth ${depth:-?}, ${time:-?}"
}

record() { # record NAME RESULT DETAIL
  summary+=("$(printf '%-34s %-6s %s' "$1" "$2" "$3")")
  echo "$1: $2 ($3)"
  [ "$2" = ok ] || failures=$((failures + 1))
}

# run_cfg CFG: run one configuration and check its expected result.
run_cfg() {
  local cfg=$1 name dir expect kind want out status tmp
  [ -f "$cfg" ] || die "no config $cfg"
  cfg=$(cd "$(dirname "$cfg")" && pwd)/$(basename "$cfg")
  name=${cfg#"$here"/}
  dir=$(dirname "$cfg")
  [ -n "$(mc_of "$dir")" ] || dir=$(dirname "$dir") # regress/, findings/, limits/: the MC module above
  expect=$(meta EXPECT "$cfg")
  kind=${expect%% *}
  want=${expect#* }
  out=$(mktemp "$work/out.XXXXXX")
  echo "== $name (expect: $expect)"
  tlc "$cfg" "$out" "$dir"; status=$tlc_status
  case "$kind" in
    pass)
      if [ "$status" = 0 ] && grep -q 'No error has been found' "$out"; then
        record "$name" ok "$(stats "$out")"
      else
        tail -60 "$out"; record "$name" FAIL "exit $status, expected no error"
      fi
      # Vacuity: the same model, with the only invariant that the effect never fires. A config
      # whose point is that the effect never fires says so with "VACUITY: skip <reason>".
      if [ "$(meta VACUITY "$cfg" | cut -d' ' -f1)" = skip ]; then
        record "$name (vacuity)" ok "skipped: $(meta VACUITY "$cfg" | cut -d' ' -f2-)"
        rm -f "$out"; return
      fi
      tmp="$(dirname "$cfg")/.vacuity-$$.cfg"
      grep -vE '^(INVARIANTS?|PROPERTY|PROPERTIES)( |$)' "$cfg" >"$tmp"
      echo "INVARIANT EffectNotReachable" >>"$tmp"
      tlc "$tmp" "$out.v" "$dir"; status=$tlc_status
      rm -f "$tmp"
      if [ "$status" = 12 ] && grep -q 'Invariant EffectNotReachable is violated' "$out.v"; then
        record "$name (vacuity)" ok "effect reachable at depth $(stats "$out.v" | sed 's/.*depth \([0-9?]*\).*/\1/')"
      else
        tail -30 "$out.v"; record "$name (vacuity)" FAIL "exit $status: the effect never fires"
      fi
      rm -f "$out.v"
      ;;
    invariant)
      if [ "$status" = 12 ] && grep -q "Invariant $want is violated" "$out" &&
         ! grep -E 'Invariant [A-Za-z]+ is violated' "$out" | grep -qv "Invariant $want is violated"; then
        record "$name" ok "$want violated, $(stats "$out")"
      else
        tail -60 "$out"; record "$name" FAIL "exit $status, expected $want violated"
      fi
      ;;
    liveness)
      if [ "$status" = 13 ] && grep -q 'Temporal properties were violated' "$out"; then
        record "$name" ok "liveness violated, $(stats "$out")"
      else
        tail -60 "$out"; record "$name" FAIL "exit $status, expected a liveness violation"
      fi
      ;;
    *) die "$name: EXPECT must be pass, invariant <Name> or liveness" ;;
  esac
  if [ -n "${TLC_KEEP_OUTPUT:-}" ]; then
    mkdir -p "$TLC_KEEP_OUTPUT"; cp "$out" "$TLC_KEEP_OUTPUT/$(echo "$name" | tr / -).out"
  fi
  rm -f "$out"
}

# models: the model directories a group runs (TLC_MODELS, or every directory holding an MC module).
models() {
  local m
  if [ -z "${TLC_MODELS:-}" ]; then
    for m in "$here"/*/; do m=$(basename "$m"); [ -z "$(mc_of "$here/$m")" ] || echo "$m"; done
    return
  fi
  for m in $TLC_MODELS; do
    [ -n "$(mc_of "$here/$m" 2>/dev/null)" ] || die "TLC_MODELS: $m is not a model directory under spec/tla"
    echo "$m"
  done
}

# group_cfgs GROUP: the configs of GROUP in the selected models, one per line.
group_cfgs() {
  local m cfg
  for m in $(models); do
    for cfg in "$here/$m"/*.cfg "$here/$m"/regress/*.cfg "$here/$m"/findings/*.cfg "$here/$m"/limits/*.cfg; do
      [ -f "$cfg" ] || continue
      [ "$(meta GROUP "$cfg")" = "$1" ] || continue
      echo "$cfg"
    done
  done
}

# run_groups GROUP...: run every config of the groups. With TLC_JOBS > 1 they share one pool of
# TLC_JOBS concurrent runs, one TLC worker each, in the order given (the larger ci configs first,
# the small regress, finding and limit configs filling the pool behind them); otherwise one at a
# time with TLC_WORKERS workers.
run_groups() {
  local g cfg cfgs=() n
  models >/dev/null # in this shell, so an unknown TLC_MODELS entry stops the check
  for g in "$@"; do
    n=${#cfgs[@]}
    while IFS= read -r cfg; do cfgs+=("$cfg"); done < <(group_cfgs "$g")
    if [ ${#cfgs[@]} = "$n" ]; then
      # finding and limit may be empty (no open finding), and so may regress for a selection of
      # models; ci never is.
      case "$g" in
        ci) die "no configs in group ci" ;;
        regress) [ -n "${TLC_MODELS:-}" ] || die "no configs in group regress"; echo "no configs in group regress" ;;
        *) echo "no configs in group $g" ;;
      esac
    fi
  done
  [ ${#cfgs[@]} -gt 0 ] || return 0
  if [ "${TLC_JOBS:-1}" -gt 1 ]; then
    run_parallel "${cfgs[@]}"
  else
    for cfg in "${cfgs[@]}"; do run_cfg "$cfg"; done
  fi
}

# run_parallel CFG...: run each config in a child check.sh, TLC_JOBS at a time, and collect their
# summaries and failures.
run_parallel() {
  local tmp i=0 f line
  tmp=$(tmpdir)
  for f in "$@"; do i=$((i + 1)); printf '%s\n' "$f" >"$tmp/$(printf '%04d' $i).path"; done
  export BIDE_TLA_CACHE="$cache"
  # Each child gets one TLC worker, a bounded heap, and its own java.io.tmpdir (TLC's parser
  # writes the standard modules there, and concurrent JVMs sharing one directory race on them), so
  # TLC_JOBS JVMs fit the machine. A child that fails exits nonzero, which xargs reports; its
  # summary says why, and a child with no summary (killed, or a script error) counts as a failure
  # of its config.
  ls "$tmp"/*.path | xargs -P "$TLC_JOBS" -I{} bash -c \
    'mkdir -p "$1.jtmp"; TLC_WORKERS=1 TLC_JOBS=1 GITHUB_STEP_SUMMARY= TLC_JAVA_OPTS="${TLC_JAVA_OPTS:--Xmx2g} -Djava.io.tmpdir=$1.jtmp" "$0" run "$(cat "$1")" >"$1.log" 2>&1' \
    "$here/check.sh" {} || true
  for f in "$tmp"/*.path; do
    sed -n '1,/^Summary:$/p' "$f.log" | grep -v '^Summary:$' | grep -v '^$' || true
    if ! grep -q '^Summary:$' "$f.log"; then
      tail -20 "$f.log"
      record "$(cat "$f")" FAIL "the check did not finish"
      continue
    fi
    while IFS= read -r line; do
      case "$line" in ''|check.sh:*) continue ;; esac
      summary+=("$line")
      case "$line" in *" FAIL "*) failures=$((failures + 1)) ;; esac
    done < <(sed -n '/^Summary:$/,$p' "$f.log" | tail -n +2)
  done
  rm -rf "$tmp"
}

self_test() {
  local s tmp
  # A stale translation is caught: edit the algorithm of a copy and check it without translating.
  s=$(specs | head -1)
  tmp=$(tmpdir)
  mkdir "$tmp/m"
  awk '{ print } /^Finish:$/ { print "  skip;"; print "FinishAfter:" }' "$s" >"$tmp/m/$(basename "$s")"
  if (here=$tmp; translation) >"$tmp/log" 2>&1 || ! grep -q '^STALE translation' "$tmp/log"; then
    cat "$tmp/log" >&2; rm -rf "$tmp"
    die "self-test: an edited algorithm was not reported as a stale translation"
  fi
  echo "self-test: a stale translation fails the check"
  # A jar with a wrong checksum is refused.
  cp "$jar" "$tmp/bad.jar"
  printf 'x' >>"$tmp/bad.jar"
  if (verify_jar "$tmp/bad.jar" "$(sed -n 's/^tla2tools *[^ ]* *\([0-9a-f]*\).*/\1/p' "$here/tools.lock")") 2>/dev/null; then
    rm -rf "$tmp"; die "self-test: a corrupted jar passed the checksum check"
  fi
  echo "self-test: a jar with the wrong SHA-256 is refused"
  rm -rf "$tmp"
}

finish() {
  echo
  echo "Summary:"
  printf '%s\n' "${summary[@]}"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    { echo '```text'; printf '%s\n' "${summary[@]}"; echo '```'; } >>"$GITHUB_STEP_SUMMARY"
  fi
  [ "$failures" = 0 ] || die "$failures check(s) failed"
}

cmd=${1:-all}
[ $# -gt 0 ] && shift
fetch
case "$cmd" in
  fetch) ;;
  translation) translation ;;
  translate) translate ;;
  self-test) self_test ;;
  ci|nightly|regress|finding|limit) run_groups "$cmd"; finish ;;
  run) [ $# -gt 0 ] || die "run: name at least one .cfg"; for c in "$@"; do run_cfg "$c"; done; finish ;;
  all) translation; run_groups ci regress finding limit; finish ;;
  *) die "unknown command $cmd (see the header of $0)" ;;
esac
