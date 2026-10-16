#!/usr/bin/env bash
# proxy-wait.sh: wait for the Go module proxy to serve a module version that was just tagged. It is
# sourced by scripts/release.sh and scripts/module-sbom.sh, not run.
#
# The proxy caches a miss. A query for a tag that is not fetchable yet (the push has not propagated,
# or the proxy asks GitHub a moment too early) is answered "not found" for the next 30 to 60 minutes,
# however often it is asked again, and polling keeps the stale answer fresh. So a release must not
# ask the proxy about a version until the tag is known to be fetchable and has had time to settle:
#
#   1. Direct: the tag is checked with GOPROXY=direct (go fetches the repository with git) and
#      GOSUMDB=off. Neither the proxy nor the checksum database (which asks the proxy) hears of the
#      version, so a miss here caches nothing. It is retried every PW_DIRECT_POLL seconds.
#   2. Settle: after a push, the proxy is not asked until PW_SETTLE seconds have passed since it.
#   3. Poll: the proxy is asked, and while it says no, the waits grow by PW_STEP seconds each time up
#      to three steps (5, 10, 15, 15, ... minutes by default), until PW_MAX seconds have passed since
#      the first question. Each wait says what it is waiting for.
#
# proxy_wait_resolve MODULE VERSION [PUSHED_AT]
#   PUSHED_AT is the epoch second of the push, or empty when the tag may have been there a while. The
#   settle delay counts from the latest of PUSHED_AT, PW_FLOOR (the epoch second the calling run
#   started: a run resumed a minute after one that pushed must not ask the proxy at once) and, when
#   the direct check had to wait for the tag, the moment it appeared. Needs
#   pw_direct MOD@VER and pw_proxy MOD@VER (commands that succeed when the version resolves). It
#   returns 1 when the version does not resolve in time; the caller says what failed.
#
# PW_DEADLINE, an epoch second, bounds the whole call (the direct wait, the settle and the proxy
# waits together): nothing sleeps past it and the call returns 1 instead. PW_MAX bounds only the
# proxy phase, from the first question.
#
# Defaults (seconds), set in the environment to change them: PW_SETTLE=300 PW_STEP=300 PW_MAX=5400
# PW_DIRECT_POLL=15 PW_DIRECT_MAX=900. PW_SLEEP and PW_NOW name the functions that sleep and read the
# clock (the self-test of scripts/release.sh replaces them).

pw_sleep() { sleep "$1"; }
pw_now() { date +%s; }
pw_log() { echo "    $*"; }

proxy_wait_resolve() {
  local mod=$1 ver=$2 pushed=${3:-} settle=${PW_SETTLE:-300} step=${PW_STEP:-300} max=${PW_MAX:-5400}
  local dpoll=${PW_DIRECT_POLL:-15} dmax=${PW_DIRECT_MAX:-900}
  local sleeper=${PW_SLEEP:-pw_sleep} clock=${PW_NOW:-pw_now}
  local t0 now waited=false n=0 delay dl=${PW_DEADLINE:-} floor=${PW_FLOOR:-}
  # 1. The tag is fetchable.
  t0=$($clock)
  until pw_direct "$mod@$ver" >/dev/null 2>&1; do
    now=$($clock)
    if [ $((now - t0)) -ge "$dmax" ] || { [ -n "$dl" ] && [ "$now" -ge "$dl" ]; }; then
      pw_log "$mod@$ver is not fetchable from its repository (GOPROXY=direct) after $(((now - t0) / 60)) minutes"
      return 1
    fi
    pw_log "waiting for the tag $ver of $mod to be fetchable from its repository (GOPROXY=direct); the proxy is not asked yet"
    waited=true
    $sleeper "$dpoll"
  done
  # 2. Let the proxy settle.
  # The settle counts from the latest of what is known: the push, the run's start, and the moment the
  # tag appeared when the direct check had to wait for it.
  local c
  for c in "$floor" "$($waited && $clock || true)"; do
    if [ -n "$c" ] && { [ -z "$pushed" ] || [ "$c" -gt "$pushed" ]; }; then pushed=$c; fi
  done
  if [ -n "$pushed" ]; then
    now=$($clock)
    if [ $((now - pushed)) -lt "$settle" ]; then
      delay=$((settle - (now - pushed)))
      if [ -n "$dl" ] && [ $((now + delay)) -ge "$dl" ]; then
        pw_log "$mod@$ver: the time allowed runs out before the proxy may be asked"
        return 1
      fi
      pw_log "$mod@$ver is fetchable; not asking proxy.golang.org for $((delay / 60))m$((delay % 60))s more, so that it does not cache a miss"
      $sleeper "$delay"
    fi
  fi
  # 3. Ask the proxy, with growing waits.
  t0=$($clock)
  while :; do
    if pw_proxy "$mod@$ver" >/dev/null 2>&1; then return 0; fi
    now=$($clock)
    n=$((n + 1))
    [ "$n" -le 3 ] && delay=$((step * n)) || delay=$((step * 3))
    if [ $((now - t0 + delay)) -gt "$max" ] || { [ -n "$dl" ] && [ $((now + delay)) -ge "$dl" ]; }; then
      pw_log "proxy.golang.org does not serve $mod@$ver after $(((now - t0) / 60)) minutes"
      return 1
    fi
    pw_log "waiting for proxy.golang.org to serve $mod@$ver (asked $n time(s)); asking again in $((delay / 60)) minutes, giving up after $((max / 60)) minutes in all"
    $sleeper "$delay"
  done
}
