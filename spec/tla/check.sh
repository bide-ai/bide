#!/usr/bin/env bash
# check.sh: fetch the pinned TLA+ tools, check that each committed PlusCal translation is up to
# date, and run TLC over the model configurations. See spec/tla/README.md.
#
#   spec/tla/check.sh                 translation check, then every "ci", "regress", "finding"
#                                     and "limit" config (what CI runs on a pull request)
#   spec/tla/check.sh ci|nightly|regress|finding|limit
#                                     the configs of one group
#   spec/tla/check.sh run FILE.cfg... the named configs, whatever their group
#   spec/tla/check.sh list GROUP...   the configs of the groups (with TLC_MODELS and TLC_DIRS), one
#                                     per line relative to spec/tla; needs no tools or Java
#   spec/tla/check.sh translation     fail if a committed translation is stale
#   spec/tla/check.sh translate       re-translate every spec in place
#   spec/tla/check.sh fetch           download and verify every tool only
#   spec/tla/check.sh self-test       prove the translation and checksum checks can fail
#   spec/tla/check.sh apalache [MODEL|FILE.cfg]...
#                                     the Apalache checks (every model's apalache/*.cfg, or those
#                                     named; see "Apalache" in spec/tla/README.md)
#
# Each .cfg names its group and its expected result in comment lines:
#   \* GROUP: ci | nightly | regress | finding | limit
#   \* EXPECT: pass | invariant <Name> | liveness
#   \* VACUITY: skip <reason>   (optional: a passing config in which the effect must never fire)
# "pass" configs are also run once with the vacuity invariant EffectNotReachable, which TLC must
# report violated: a model in which the effect never fires satisfies every safety property.
#
# An Apalache configuration (spec/tla/<model>/apalache/*.cfg) names its module and its checks in
# comment lines, each check an expected result and the arguments of "apalache-mc check":
#   \* SPEC: <module>.tla   (in the model's directory)
#   \* CHECK: pass | <arguments>
#   \* CHECK: invariant <Name> | <arguments>
#   \* CHECK: typecheck |   (Apalache's type checker only)
# A configuration in a model's apalache/tlc/ directory is for TLC instead: TLC checks the module
# its SPEC line names with it and must find no error (an inductive invariant that is also a
# plain invariant of every reachable state).
#
# Needs Java 11 or later (JAVA_HOME or java on PATH; Apalache needs 17 or later), curl, tar, and
# sha256sum or shasum.
# Environment: BIDE_TLA_CACHE (tool cache; default ~/.cache/bide-tla), BIDE_APALACHE_CACHE (where
# the Apalache archive is kept; default the tool cache), APALACHE_JAVA_OPTS (default -Xmx8g),
# APALACHE_PROGRESS (seconds between progress lines of a running Apalache check; default 300, 0 for
# none),
# TLC_WORKERS (default auto),
# TLC_JOBS (default 1: how many configs run at a time; above 1, each runs with one TLC worker),
# TLC_MODELS (default every model: the model directories whose configs a group runs, such as
# "claims toolcall"), TLC_DIRS (default every directory: the configuration directories a group
# runs, each a model's directory or its regress, findings or limits directory, such as
# "claims claims/limits"; CI's Models shards), TLC_JAVA_OPTS (extra JVM options).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cache=${BIDE_TLA_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/bide-tla}
workers=${TLC_WORKERS:-auto}
java=${JAVA_HOME:+$JAVA_HOME/bin/}java
summary=()
failures=0

# Every temporary file and directory (TLC metadirs, outputs, translation copies, the unpacked
# Apalache and its output and JVM temporary directories) lives under one work directory, removed on
# exit, failure and interrupt, so a killed run leaves no checker state behind. A vacuity config is
# written beside its config (see run_cfg) and removed the same way.
work=$(mktemp -d "${TMPDIR:-/tmp}/bide-tla.XXXXXX")
cleanup() {
  if [ -n "${tlc_pid:-}" ]; then kill "$tlc_pid" 2>/dev/null || true; wait "$tlc_pid" 2>/dev/null || true; fi
  if [ -n "${progress_pid:-}" ]; then kill "$progress_pid" 2>/dev/null || true; wait "$progress_pid" 2>/dev/null || true; fi
  if [ -n "${apalache_pid:-}" ]; then kill "$apalache_pid" 2>/dev/null || true; wait "$apalache_pid" 2>/dev/null || true; fi
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

# fetch_tool NAME DIR: download NAME's file as tools.lock pins it ("name version sha256 url") into
# DIR, unless it is there, verify its SHA-256, and set $fetched to its path. It runs in this shell,
# not a command substitution, so any failure stops the check.
fetch_tool() {
  local name version sum url
  name=; version=; sum=; url=
  while read -r name version sum url; do
    [ "$name" = "$1" ] && break
    name=
  done < "$here/tools.lock"
  [ -n "$name" ] && [ -n "$url" ] || die "tools.lock names no $1"
  mkdir -p "$2"
  fetched="$2/$name-$version.${url##*.}"
  if [ ! -f "$fetched" ]; then
    echo "fetching $name $version"
    curl -fsSL --retry 3 -o "$fetched.part" "$url"
    mv "$fetched.part" "$fetched"
  fi
  verify_jar "$fetched" "$sum"
}

# fetch: the TLA+ tools (tla2tools.jar) in the cache, verified; set $jar.
fetch() {
  fetch_tool tla2tools "$cache"
  jar=$fetched
  "$java" -version >/dev/null 2>&1 || die "no java (set JAVA_HOME or put java on PATH)"
}

# fetch_apalache: the Apalache release archive in its cache, verified, unpacked into the work
# directory (a fresh copy each run, so only verified bytes run); set $apalache.
fetch_apalache() {
  fetch_tool apalache "${BIDE_APALACHE_CACHE:-$cache}"
  mkdir -p "$work/apalache"
  tar -xzf "$fetched" -C "$work/apalache" --strip-components=1
  apalache=$work/apalache/lib/apalache.jar
  [ -f "$apalache" ] || die "$fetched holds no lib/apalache.jar"
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
  local cfg=$1 out=$2 dir=$3 module=${4:-} meta_dir
  meta_dir=$(tmpdir)
  (cd "$dir" && exec "$java" -XX:+UseParallelGC ${TLC_JAVA_OPTS:-} -cp "$jar" tlc2.TLC \
      -workers "$workers" -metadir "$meta_dir" -config "$cfg" "${module:-$(mc_of "$dir")}") >"$out" 2>&1 &
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

# dirs: fail unless each TLC_DIRS entry is a configuration directory group_cfgs reads (a model's
# directory, or its regress, findings or limits directory), so a misspelt entry cannot select
# nothing.
dirs() {
  local d
  for d in ${TLC_DIRS:-}; do
    case "$d" in
      */regress|*/findings|*/limits) [ -n "$(mc_of "$here/${d%/*}" 2>/dev/null)" ] && [ -d "$here/$d" ] ;;
      */*) false ;;
      *) [ -n "$(mc_of "$here/$d" 2>/dev/null)" ] ;;
    esac || die "TLC_DIRS: $d is not a model directory under spec/tla or its regress, findings or limits directory"
  done
}

# in_dirs CFG: CFG's directory is in TLC_DIRS (always, when TLC_DIRS is empty).
in_dirs() {
  local d rel
  [ -n "${TLC_DIRS:-}" ] || return 0
  rel=$(dirname "${1#"$here"/}")
  for d in $TLC_DIRS; do [ "$d" = "$rel" ] && return 0; done
  return 1
}

# group_cfgs GROUP: the configs of GROUP in the selected models and directories, one per line.
group_cfgs() {
  local m cfg
  for m in $(models); do
    for cfg in "$here/$m"/*.cfg "$here/$m"/regress/*.cfg "$here/$m"/findings/*.cfg "$here/$m"/limits/*.cfg; do
      [ -f "$cfg" ] || continue
      [ "$(meta GROUP "$cfg")" = "$1" ] || continue
      in_dirs "$cfg" || continue
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
  models >/dev/null # in this shell, so an unknown TLC_MODELS or TLC_DIRS entry stops the check
  dirs
  for g in "$@"; do
    n=${#cfgs[@]}
    while IFS= read -r cfg; do cfgs+=("$cfg"); done < <(group_cfgs "$g")
    if [ ${#cfgs[@]} = "$n" ]; then
      # finding and limit may be empty (no open finding), and so may regress for a selection of
      # models, and ci and regress for a selection of directories (a CI shard; .github/scripts/
      # shards.sh checks that the shards together run every config); ci never is otherwise.
      case "$g" in
        ci) [ -n "${TLC_DIRS:-}" ] || die "no configs in group ci"; echo "no configs in group ci" ;;
        regress) [ -n "${TLC_MODELS:-}${TLC_DIRS:-}" ] || die "no configs in group regress"; echo "no configs in group regress" ;;
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

# apalache_progress ODIR START: one line on how far the running check has got, from Apalache's
# detailed log: the step, the transition and how many it has (enabled ones are checked against
# every invariant conjunct, disabled ones are discarded), and the invariant conjunct it checks.
# Output only: no result depends on it.
apalache_progress() {
  local log total last step tr en dis inv
  # Under pipefail a grep that matches nothing fails its pipeline: every one ends with || true.
  log=$(find "$1" -name detailed.log 2>/dev/null | head -1 || true)
  [ -n "$log" ] && [ -f "$log" ] || return 0
  total=$(sed -n 's/.*Found \([0-9]*\) transitions.*/\1/p' "$log" | tail -1 || true)
  last=$(grep -o 'Step [0-9]*: Transition #[0-9]*' "$log" | tail -1 || true)
  [ -n "$last" ] || { echo "   progress: preparing ($(( ($(date +%s) - $2) / 60 )) min)"; return 0; }
  step=$(echo "$last" | sed 's/Step \([0-9]*\):.*/\1/')
  tr=$(echo "$last" | sed 's/.*#//')
  en=$(grep -c "Step $step: Transition #[0-9]* is enabled" "$log" || true)
  dis=$(grep -c "Step $step: Transition #[0-9]* is disabled" "$log" || true)
  inv=$(grep -o 'Checking state invariant [0-9]*' "$log" | tail -1 | sed 's/.* //' || true)
  echo "   progress: step $step, transition $tr of ${total:-?} ($en enabled, $dis disabled so far), invariant conjunct ${inv:-?}, $(( ($(date +%s) - $2) / 60 )) min"
}

# apalache_run OUT DIR SPEC CFG ARG...: run "apalache-mc check" on SPEC in DIR with CFG, its output
# directory and JVM temporary directory under the work directory; set apalache_status to its exit
# status (0: no error, 12: an invariant violated). Like tlc, it runs in the background and is
# waited for, so an interrupt stops it at once.
apalache_pid=
apalache_run() {
  local out=$1 dir=$2 spec=$3 cfg=$4 odir
  shift 4
  odir=$(tmpdir)
  # shellcheck disable=SC2086 # APALACHE_JAVA_OPTS is a list of options
  if [ -z "$cfg" ]; then # a type check only
    (cd "$dir" && exec "$java" ${APALACHE_JAVA_OPTS:--Xmx8g} -Djava.io.tmpdir="$odir" -jar "$apalache" \
        typecheck --out-dir="$odir" "$spec") >"$out" 2>&1 &
  else
    (cd "$dir" && exec "$java" ${APALACHE_JAVA_OPTS:--Xmx8g} -Djava.io.tmpdir="$odir" -jar "$apalache" \
        check --out-dir="$odir" --config="$cfg" --no-deadlock "$@" "$spec") >"$out" 2>&1 &
  fi
  apalache_pid=$!
  # A progress line every APALACHE_PROGRESS seconds (default 300; 0 for none), so a long check
  # shows how far it has got in a live CI log.
  progress_pid=
  if [ "${APALACHE_PROGRESS:-300}" -gt 0 ]; then
    # The sleep runs in the background and is waited for, so the TERM that stops the loop also
    # stops the sleep (the trap) instead of leaving it orphaned.
    (start=$(date +%s); nap=
     trap '[ -z "$nap" ] || kill "$nap" 2>/dev/null; exit 0' TERM
     while :; do
       sleep "${APALACHE_PROGRESS:-300}" & nap=$!; wait "$nap" || true; nap=
       kill -0 "$apalache_pid" 2>/dev/null || exit 0; apalache_progress "$odir" "$start"
     done) &
    progress_pid=$!
  fi
  apalache_status=0
  wait "$apalache_pid" || apalache_status=$?
  apalache_pid=
  if [ -n "$progress_pid" ]; then kill "$progress_pid" 2>/dev/null || true; wait "$progress_pid" 2>/dev/null || true; fi
  progress_pid=
  # The counterexample, if any, for the summary's caller and for APALACHE_KEEP_OUTPUT.
  find "$odir" -name 'violation1.tla' -exec cp {} "$out.trace" \; 2>/dev/null || true
  rm -rf "$odir"
}

# run_apalache_cfg CFG: run every CHECK line of an Apalache configuration and check its result.
run_apalache_cfg() {
  local cfg=$1 name dir spec line expect args out kind want t0 secs
  [ -f "$cfg" ] || die "no config $cfg"
  cfg=$(cd "$(dirname "$cfg")" && pwd)/$(basename "$cfg")
  name=${cfg#"$here"/}
  dir=$(dirname "$(dirname "$cfg")")
  spec=$(meta SPEC "$cfg")
  [ -n "$spec" ] && [ -f "$dir/$spec" ] || die "$name: SPEC must name a module in $dir"
  grep -q '^\\\* CHECK:' "$cfg" || die "$name: no CHECK line"
  while IFS= read -r line; do
    expect=$(echo "${line%%|*}" | sed 's/ *$//')
    args=${line#*|}
    kind=${expect%% *}
    want=${expect#* }
    out=$(mktemp "$work/out.XXXXXX")
    echo "== $name:$args (expect: $expect)"
    t0=$(date +%s)
    if [ "$kind" = typecheck ]; then
      apalache_run "$out" "$dir" "$spec" ""
    else
      # shellcheck disable=SC2086 # args is the list of arguments the CHECK line gives
      apalache_run "$out" "$dir" "$spec" "$cfg" $args
    fi
    secs=$(( $(date +%s) - t0 ))s
    case "$kind" in
      pass)
        # Exit status 0 alone is not a pass: a violated ASSUME (ClaimsInductive's Drivers guard)
        # also exits 0, with the outcome ExecutionsTooShort.
        if [ "$apalache_status" = 0 ] && grep -q 'The outcome is: NoError' "$out"; then
          record "$name:$args" ok "no error, $secs"
        else
          tail -40 "$out"; [ ! -f "$out.trace" ] || cat "$out.trace"
          record "$name:$args" FAIL "exit $apalache_status, expected no error"
        fi
        ;;
      invariant)
        if [ "$apalache_status" = 12 ] && grep -q 'The outcome is: Error' "$out" &&
           echo "$args" | grep -qE -- "--inv=$want( |$)"; then
          record "$name:$args" ok "$want violated, $secs"
        else
          tail -40 "$out"; record "$name:$args" FAIL "exit $apalache_status, expected $want violated"
        fi
        ;;
      typecheck)
        if [ "$apalache_status" = 0 ] && grep -q 'Type checker \[OK\]' "$out"; then
          record "$name: typecheck" ok "types check, $secs"
        else
          tail -40 "$out"; record "$name: typecheck" FAIL "exit $apalache_status, expected the types to check"
        fi
        ;;
      *) die "$name: CHECK must expect pass, invariant <Name> or typecheck" ;;
    esac
    if [ -n "${APALACHE_KEEP_OUTPUT:-}" ]; then
      mkdir -p "$APALACHE_KEEP_OUTPUT"
      cp "$out" "$APALACHE_KEEP_OUTPUT/$(echo "$name" | tr / -).$(echo "$args" | tr -c 'A-Za-z0-9=\n' _).out"
      [ ! -f "$out.trace" ] || cp "$out.trace" "$APALACHE_KEEP_OUTPUT/$(echo "$name" | tr / -).$(echo "$args" | tr -c 'A-Za-z0-9=\n' _).trace.tla"
    fi
    rm -f "$out" "$out.trace"
  done < <(sed -n 's/^\\\* CHECK: *//p' "$cfg")
}

# run_tlc_spec_cfg CFG: a configuration in a model's apalache/tlc/ directory: TLC checks the
# module its SPEC line names (an Apalache module, such as ClaimsInductive.tla, whose inductive
# invariant must also be a plain invariant of every reachable state) and must find no error.
run_tlc_spec_cfg() {
  local cfg=$1 name dir spec out
  cfg=$(cd "$(dirname "$cfg")" && pwd)/$(basename "$cfg")
  name=${cfg#"$here"/}
  dir=$(dirname "$(dirname "$(dirname "$cfg")")")
  spec=$(meta SPEC "$cfg")
  [ -n "$spec" ] && [ -f "$dir/$spec" ] || die "$name: SPEC must name a module in $dir"
  out=$(mktemp "$work/out.XXXXXX")
  echo "== $name (TLC on $spec, expect: pass)"
  tlc "$cfg" "$out" "$dir" "$spec"
  if [ "$tlc_status" = 0 ] && grep -q 'No error has been found' "$out"; then
    record "$name" ok "$(stats "$out")"
  else
    tail -60 "$out"; record "$name" FAIL "exit $tlc_status, expected no error"
  fi
  rm -f "$out"
}

# apalache_cfgs [MODEL|FILE.cfg]...: the Apalache configurations named, or every model's.
apalache_cfgs() {
  local a cfg
  if [ $# = 0 ]; then
    for cfg in "$here"/*/apalache/*.cfg "$here"/*/apalache/tlc/*.cfg; do [ -f "$cfg" ] && echo "$cfg"; done
    return
  fi
  for a in "$@"; do
    if [ -f "$a" ]; then echo "$a"
    elif [ -d "$here/$a/apalache" ]; then
      for cfg in "$here/$a"/apalache/*.cfg "$here/$a"/apalache/tlc/*.cfg; do [ -f "$cfg" ] && echo "$cfg"; done
    else die "apalache: $a is neither a .cfg nor a model with an apalache/ directory"
    fi
  done
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
if [ "$cmd" = list ]; then
  [ $# -gt 0 ] || die "list: name at least one group"
  models >/dev/null
  dirs
  for g in "$@"; do
    case "$g" in ci|nightly|regress|finding|limit) ;; *) die "list: unknown group $g" ;; esac
    group_cfgs "$g" | sed "s|^$here/||"
  done
  exit 0
fi
fetch
case "$cmd" in
  fetch) fetch_apalache ;;
  translation) translation ;;
  translate) translate ;;
  self-test) self_test ;;
  ci|nightly|regress|finding|limit) run_groups "$cmd"; finish ;;
  run) [ $# -gt 0 ] || die "run: name at least one .cfg"; for c in "$@"; do run_cfg "$c"; done; finish ;;
  apalache)
    cfgs=$(apalache_cfgs "$@")
    [ -n "$cfgs" ] || die "apalache: no configurations"
    case "$cfgs" in */apalache/[!t]*|*/apalache/t[!l]*) fetch_apalache ;; esac
    for c in $cfgs; do
      case "$c" in */apalache/tlc/*) run_tlc_spec_cfg "$c" ;; *) run_apalache_cfg "$c" ;; esac
    done
    finish ;;
  all) translation; run_groups ci regress finding limit; finish ;;
  *) die "unknown command $cmd (see the header of $0)" ;;
esac
