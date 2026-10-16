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
#   PUSHED_AT is the epoch second of the push, or empty when the tag may have been there a while (the
#   settle delay then applies only if the direct check had to wait for the tag to appear). Needs
#   pw_direct MOD@VER and pw_proxy MOD@VER (commands that succeed when the version resolves). It
#   returns 1 when the version does not resolve in time; the caller says what failed.
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
  local t0 now waited=false n=0 delay
  # 1. The tag is fetchable.
  t0=$($clock)
  until pw_direct "$mod@$ver" >/dev/null 2>&1; do
    now=$($clock)
    if [ $((now - t0)) -ge "$dmax" ]; then
      pw_log "$mod@$ver is not fetchable from its repository (GOPROXY=direct) after $((dmax / 60)) minutes"
      return 1
    fi
    pw_log "waiting for the tag $ver of $mod to be fetchable from its repository (GOPROXY=direct); the proxy is not asked yet"
    waited=true
    $sleeper "$dpoll"
  done
  # 2. Let the proxy settle.
  if $waited && [ -z "$pushed" ]; then pushed=$($clock); fi
  if [ -n "$pushed" ]; then
    now=$($clock)
    if [ $((now - pushed)) -lt "$settle" ]; then
      delay=$((settle - (now - pushed)))
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
    if [ $((now - t0 + delay)) -gt "$max" ]; then
      pw_log "proxy.golang.org does not serve $mod@$ver after $(((now - t0) / 60)) minutes"
      return 1
    fi
    pw_log "waiting for proxy.golang.org to serve $mod@$ver (asked $n time(s)); asking again in $((delay / 60)) minutes, giving up after $((max / 60)) minutes in all"
    $sleeper "$delay"
  done
}
