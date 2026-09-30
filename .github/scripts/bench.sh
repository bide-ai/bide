#!/usr/bin/env bash
# bench.sh: run the cmd/bench scenarios and summarize them for .github/workflows/bench.yml.
#
#   bench.sh run REPEATS OUTDIR LABEL=BINARY [LABEL=BINARY...]
#       Runs every scenario REPEATS times with each binary, interleaved: for each repeat and
#       scenario, the binaries run one after another in the order given (base, head, base, head,
#       ...), so drift in the machine's speed during the job reaches every binary alike. Each
#       output goes to OUTDIR/LABEL/SCENARIO-N.txt. A run that reports errors fails.
#   bench.sh summary OUTDIR LABEL REPEATS
#       Markdown table of the medians of one binary's runs.
#
#   Latency is reported as the mean (concurrency / throughput, by Little's law: the harness keeps
#   a fixed number of runs in flight), p90 and p99. p50 stays in the raw output but is not
#   reported: in this closed-loop harness it is bimodal, sitting on a scheduling cliff, so one
#   binary's p50 moves by an order of magnitude between runs.
#   bench.sh compare OUTDIR BASE_LABEL HEAD_LABEL REPEATS
#       Markdown table of the medians of two binaries' runs and the change from base to head.
#   bench.sh cpu
#       The machine: vCPU count and CPU model.
#   bench.sh --self-test
#       Checks the parsing, medians and percentage change against fixed outputs.
set -euo pipefail

# name|flags of each scenario, in the order they run.
SCENARIOS="overhead|-runs 5000 -concurrency 256
fanout|-runs 20000 -concurrency 5000 -latency 50ms"

# Metrics: key|heading|unit|which direction is better.
METRICS="wall|Wall-clock|ms|lower
rps|Runs/s||higher
recs|Journal records/s||higher
mean|Mean latency|ms|lower
p90|p90|ms|lower
p99|p99|ms|lower
gor|Peak goroutines||lower
heap|Heap delta|MB|lower"

die() { echo "bench: error: $*" >&2; exit 1; }

cpu() {
  local n model
  if [ -r /proc/cpuinfo ]; then
    n=$(nproc); model=$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | sed 's/^ //')
  else
    n=$(sysctl -n hw.ncpu 2>/dev/null || echo "?"); model=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)
  fi
  echo "$n vCPU, $model"
}

# ms: durations as printed by Go (µs, ms, s) to milliseconds, one per line.
ms() { awk '{ v=$1; if (v ~ /µs$/) { sub(/µs$/,"",v); v=v/1000 } else if (v ~ /ms$/) { sub(/ms$/,"",v) } else if (v ~ /s$/) { sub(/s$/,"",v); v=v*1000 } print v }'; }
# median: the median of the numbers on stdin; it fails on no input.
median() { sort -g | awk '{ a[NR]=$1 } END { if (NR == 0) exit 1; if (NR%2) print a[(NR+1)/2]; else print (a[NR/2]+a[NR/2+1])/2 }'; }

# values dir scenario metric: one value per run, in the metric's unit.
values() {
  local files=("$1/$2"-*.txt)
  [ -e "${files[0]}" ] || die "no output for $2 in $1"
  case $3 in
    wall) grep -ho 'elapsed=[^ ]*' "${files[@]}" | cut -d= -f2 | ms ;;
    rps) grep -ho 'throughput=[0-9]*' "${files[@]}" | cut -d= -f2 ;;
    recs) grep -ho '([0-9]* journal records/s' "${files[@]}" | tr -dc '0-9\n' ;;
    p90|p99) grep -ho "$3=[^ ]*" "${files[@]}" | cut -d= -f2 | ms ;;
    mean) # concurrency / throughput, in ms, one per run
      local f c t
      for f in "${files[@]}"; do
        c=$(grep -o 'concurrency=[0-9]*' "$f" | head -1 | cut -d= -f2)
        t=$(grep -o 'throughput=[0-9]*' "$f" | head -1 | cut -d= -f2)
        [ -n "$c" ] && [ -n "$t" ] && [ "$t" -gt 0 ] || die "no concurrency or throughput in $f"
        awk -v c="$c" -v t="$t" 'BEGIN { printf "%.6f\n", c / t * 1000 }'
      done ;;
    gor) grep -ho 'goroutines=[0-9]*' "${files[@]}" | cut -d= -f2 ;;
    heap) grep -ho 'heap alloc delta=[0-9.]* MB' "${files[@]}" | grep -o '[0-9.]*' ;;
    *) die "unknown metric $3" ;;
  esac
}

# med dir scenario metric: the median over the runs, rounded for the table.
med() {
  local v; v=$(values "$1" "$2" "$3" | median) || die "no $3 values for $2 in $1"
  case $3 in
    wall|mean|p90|p99) awk -v v="$v" 'BEGIN { if (v >= 100) printf "%.0f", v; else if (v >= 10) printf "%.1f", v; else printf "%.2f", v }' ;;
    heap) awk -v v="$v" 'BEGIN { printf "%.1f", v }' ;;
    *) awk -v v="$v" 'BEGIN { printf "%.0f", v }' ;;
  esac
}

# change base head: the percentage change from base to head, signed, one decimal.
change() { awk -v b="$1" -v h="$2" 'BEGIN { if (b == 0) { print "n/a"; exit } printf "%+.1f%%", (h - b) / b * 100 }'; }

run() {
  local n=$1 out=$2; shift 2
  [[ $n =~ ^[1-9][0-9]*$ ]] || die "repeats must be a positive integer, not $n"
  local pair label bin i name flags f
  for pair in "$@"; do
    label=${pair%%=*}; bin=${pair#*=}
    [ -x "$bin" ] || die "$bin is not an executable"
    mkdir -p "$out/$label"
  done
  for i in $(seq 1 "$n"); do
    while IFS='|' read -r name flags; do
      for pair in "$@"; do
        label=${pair%%=*}; bin=${pair#*=}; f=$out/$label/$name-$i.txt
        # shellcheck disable=SC2086
        "$bin" $flags >"$f"
        grep -q '^errors=0$' "$f" || { cat "$f"; die "$label $name run $i reported errors"; }
      done
    done <<<"$SCENARIOS"
  done
}

scenario_notes() {
  local name flags notes=""
  while IFS='|' read -r name flags; do notes="$notes${notes:+; }$name: \`$flags\`"; done <<<"$SCENARIOS"
  echo "$notes."
}

latency_note() {
  echo "Mean latency is concurrency / throughput; p50 is in the raw output only, since this closed-loop harness's p50 is bimodal (it sits on a scheduling cliff)."
}

summary() {
  local out=$1 label=$2 n=$3 name flags
  echo "| Scenario | Wall-clock | Runs/s | Journal records/s | Mean latency | p90 | p99 | Peak goroutines | Heap delta |"
  echo "|---|---|---|---|---|---|---|---|---|"
  while IFS='|' read -r name flags; do
    echo "| $name | $(med "$out/$label" "$name" wall) ms | $(med "$out/$label" "$name" rps) | $(med "$out/$label" "$name" recs) | $(med "$out/$label" "$name" mean) ms | $(med "$out/$label" "$name" p90) ms | $(med "$out/$label" "$name" p99) ms | $(med "$out/$label" "$name" gor) | $(med "$out/$label" "$name" heap) MB |"
  done <<<"$SCENARIOS"
  echo
  echo "$n runs per scenario; each figure is the median. $(latency_note) $(scenario_notes)"
}

compare() {
  local out=$1 base=$2 head=$3 n=$4 name flags key heading unit better b h
  echo "| Scenario | Metric | $base | $head | Change | Better |"
  echo "|---|---|---|---|---|---|"
  while IFS='|' read -r name flags; do
    while IFS='|' read -r key heading unit better; do
      b=$(med "$out/$base" "$name" "$key"); h=$(med "$out/$head" "$name" "$key")
      echo "| $name | $heading | $b${unit:+ $unit} | $h${unit:+ $unit} | $(change "$b" "$h") | $better |"
    done <<<"$METRICS"
  done <<<"$SCENARIOS"
  echo
  echo "$n runs per scenario and binary, interleaved on one machine; each figure is the median, and the change is from $base to $head. $(latency_note) $(scenario_notes)"
}

self_test() {
  local tmp fail=false got
  tmp=$(mktemp -d)
  mkdir -p "$tmp/a" "$tmp/b"
  # Three runs per binary; the medians are the middle lines. Units vary as Go prints them.
  local e t r p50 p90 p99 g hp i
  i=0
  for row in "900µs 1000 5000 500µs 800µs 40ms 300 10.0" "1.5s 3000 15000 2ms 3ms 60ms 310 30.0" "1.2s 2000 10000 1.5ms 2ms 50ms 305 20.0"; do
    i=$((i + 1)); read -r e t r p50 p90 p99 g hp <<<"$row"
    printf 'runs=5000 concurrency=256 sim-latency=0s\nelapsed=%s throughput=%s runs/s (%s journal records/s, in-memory store; 25000 records)\nrun latency: p50=%s p90=%s p99=%s max=90ms\npeak goroutines=%s  heap alloc delta=%s MB  total alloc=100.0 MB  numGC=3\nerrors=0\n' \
      "$e" "$t" "$r" "$p50" "$p90" "$p99" "$g" "$hp" >"$tmp/a/overhead-$i.txt"
  done
  for i in 1 2 3; do sed -e 's/throughput=[0-9]*/throughput=2200/' -e 's/elapsed=[^ ]*/elapsed=1.8s/' "$tmp/a/overhead-$i.txt" >"$tmp/b/overhead-$i.txt"; done
  check() { [ "$2" = "$3" ] || { echo "self-test: $1 = $2, want $3"; fail=true; }; }
  check "wall median" "$(med "$tmp/a" overhead wall)" 1200
  check "wall of 900µs" "$(values "$tmp/a" overhead wall | head -1)" 0.9
  check "runs/s median" "$(med "$tmp/a" overhead rps)" 2000
  check "records/s median" "$(med "$tmp/a" overhead recs)" 10000
  check "mean median" "$(med "$tmp/a" overhead mean)" 128
  check "mean of 256 at 1000/s" "$(values "$tmp/a" overhead mean | head -1)" 256.000000
  check "p90 median" "$(med "$tmp/a" overhead p90)" 2.00
  check "p99 median" "$(med "$tmp/a" overhead p99)" 50.0
  check "goroutines median" "$(med "$tmp/a" overhead gor)" 305
  check "heap median" "$(med "$tmp/a" overhead heap)" 20.0
  check "even-count median" "$(printf '4\n1\n3\n2\n' | median)" 2.5
  check "change up" "$(change 2000 2200)" "+10.0%"
  check "change down" "$(change 1200 1800)" "+50.0%"
  check "change fall" "$(change 200 150)" "-25.0%"
  got=$(SCENARIOS="overhead|-runs 1"; compare "$tmp" a b 3)
  echo "$got" | grep -qF '| overhead | Runs/s | 2000 | 2200 | +10.0% | higher |' || { echo "self-test: compare row for runs/s missing:"; echo "$got"; fail=true; }
  echo "$got" | grep -qF '| overhead | Wall-clock | 1200 ms | 1800 ms | +50.0% | lower |' || { echo "self-test: compare row for wall-clock missing:"; echo "$got"; fail=true; }
  # run interleaves the binaries and refuses a run that reports errors.
  printf '#!/bin/sh\necho "$0" >>%s/order\necho errors=0\n' "$tmp" >"$tmp/x"; cp "$tmp/x" "$tmp/y"; chmod +x "$tmp/x" "$tmp/y"
  (SCENARIOS="s1|-a
s2|-b"; run 2 "$tmp/out" base="$tmp/x" head="$tmp/y")
  check "run order" "$(sed "s|$tmp/||" "$tmp/order" | tr '\n' ' ')" "x y x y x y x y "
  check "run outputs" "$(cd "$tmp/out" && ls base head | tr '\n' ' ')" "base: s1-1.txt s1-2.txt s2-1.txt s2-2.txt  head: s1-1.txt s1-2.txt s2-1.txt s2-2.txt "
  printf '#!/bin/sh\necho errors=1\n' >"$tmp/z"; chmod +x "$tmp/z"
  (SCENARIOS="s1|-a"; run 1 "$tmp/out2" bad="$tmp/z") >/dev/null 2>&1 && { echo "self-test: a run reporting errors was accepted"; fail=true; }
  rm -rf "$tmp"
  $fail && return 1
  echo "self-test: ok"
}

case "${1:-}" in
  run) shift; [ $# -ge 3 ] || die "usage: bench.sh run REPEATS OUTDIR LABEL=BINARY..."; run "$@" ;;
  summary) [ $# -eq 4 ] || die "usage: bench.sh summary OUTDIR LABEL REPEATS"; summary "$2" "$3" "$4" ;;
  compare) [ $# -eq 5 ] || die "usage: bench.sh compare OUTDIR BASE_LABEL HEAD_LABEL REPEATS"; compare "$2" "$3" "$4" "$5" ;;
  cpu) cpu ;;
  --self-test) self_test ;;
  *) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
