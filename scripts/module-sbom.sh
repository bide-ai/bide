#!/usr/bin/env bash
# module-sbom.sh: the SBOM of bide's published Go modules at a release, as SPDX JSON.
#
#   scripts/module-sbom.sh vX.Y.Z OUT.json [--wait MINUTES]
#   scripts/module-sbom.sh --self-test
#
# The per-archive SBOMs of a release list what is compiled into the bide binary. This one lists what
# an application gets with `go get`: it builds the consumer module scripts/release.sh builds for its
# last check (one main package that imports the core and every published library, with the published
# commands as tools), resolves it at vX.Y.Z through the public module proxy and checksum database,
# and has syft list every module in its tidy go.mod. The bide modules appear at vX.Y.Z, each
# third-party module at the version a consumer selects, as a package URL (pkg:golang/...), with the
# license syft finds in that module's files.
#
# --wait MINUTES waits until the proxy serves every published module at vX.Y.Z (the release workflow
# runs before scripts/release.sh has pushed the nested tags), asking the proxy sparingly (see
# scripts/proxy-wait.sh). It needs syft on the path.
set -euo pipefail

ROOT_MODULE=github.com/bide-ai/bide
PROXY=https://proxy.golang.org
here=$(cd "$(dirname "$0")" && pwd)

die() { echo "module-sbom: error: $*" >&2; exit 1; }
usage() { sed -n '2,4p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

# self_test: the script on fake go and syft commands, run from another directory, writes OUT where
# the caller named it, for a bare file name and a relative path as for an absolute one. It needs no
# network.
self_test() {
  local tmp mods m ok=true; tmp=$(mktemp -d)
  mkdir -p "$tmp/bin" "$tmp/cwd/sub"
  cat > "$tmp/bin/go" <<'GO'
#!/usr/bin/env bash
# A fake go: every module resolves, and the consumer selects each module at $FAKE_VERSION.
if [ "$1 $2" = "list -m" ] && [ "${3:-}" = "-f" ]; then for m in $FAKE_MODULES; do echo "$m $FAKE_VERSION"; done; exit 0; fi
exit 0
GO
  cat > "$tmp/bin/syft" <<'SYFT'
#!/usr/bin/env bash
# A fake syft: writes an SBOM of $FAKE_MODULES at $FAKE_VERSION to the spdx-json= output.
for a in "$@"; do case "$a" in spdx-json=*) out=${a#spdx-json=} ;; esac; done
for m in $FAKE_MODULES; do printf '%s %s\n' "$m" "$FAKE_VERSION"; done |
  jq -Rn '{packages: [inputs | split(" ") | {name: .[0], versionInfo: .[1]}]}' > "$out"
SYFT
  chmod +x "$tmp/bin/go" "$tmp/bin/syft"
  mods="$ROOT_MODULE"
  for m in $("$here/release.sh" --list-published); do mods="$mods $ROOT_MODULE/$m"; done
  for out in sbom.json sub/sbom.json "$tmp/cwd/abs.json"; do
    rm -f "$tmp/cwd/$out"
    if ! (cd "$tmp/cwd" && PATH="$tmp/bin:$PATH" FAKE_VERSION=v9.9.9 FAKE_MODULES="$mods" "$here/module-sbom.sh" v9.9.9 "$out" >"$tmp/log" 2>&1); then
      echo "self-test: module-sbom.sh $out failed:"; sed 's/^/    /' "$tmp/log"; ok=false; continue
    fi
    case "$out" in /*) want=$out ;; *) want=$tmp/cwd/$out ;; esac
    [ -s "$want" ] || { echo "self-test: the SBOM for OUT=$out is not at $want"; ok=false; }
  done
  # A relative TMPDIR: the scratch directory is still found after the script changes directory.
  (cd "$tmp/cwd" && mkdir -p rel && PATH="$tmp/bin:$PATH" TMPDIR=rel FAKE_VERSION=v9.9.9 FAKE_MODULES="$mods" "$here/module-sbom.sh" v9.9.9 rel.json >"$tmp/log" 2>&1) \
    && [ -s "$tmp/cwd/rel.json" ] || { echo "self-test: a relative TMPDIR broke the run:"; sed 's/^/    /' "$tmp/log"; ok=false; }
  if (cd "$tmp/cwd" && PATH="$tmp/bin:$PATH" FAKE_VERSION=v9.9.9 FAKE_MODULES="$mods" "$here/module-sbom.sh" v9.9.9 - >"$tmp/log" 2>&1); then
    echo "self-test: OUT - was accepted"; ok=false
  fi
  # A pre-release version is a release version; garbage after the patch number is not.
  (cd "$tmp/cwd" && PATH="$tmp/bin:$PATH" FAKE_VERSION=v9.9.9-rc.1 FAKE_MODULES="$mods" "$here/module-sbom.sh" v9.9.9-rc.1 pre.json >"$tmp/log" 2>&1) \
    && [ -s "$tmp/cwd/pre.json" ] || { echo "self-test: a pre-release version was refused:"; sed 's/^/    /' "$tmp/log"; ok=false; }
  local bad
  for bad in v9.9 v9.9.9x v9.9.9- 'v9.9.9-a b' 9.9.9 'v9.9.9;rm'; do
    if (cd "$tmp/cwd" && PATH="$tmp/bin:$PATH" FAKE_VERSION=v9.9.9 FAKE_MODULES="$mods" "$here/module-sbom.sh" "$bad" bad.json >"$tmp/log" 2>&1); then
      echo "self-test: version '$bad' was accepted"; ok=false
    fi
  done
  rm -rf "$tmp"
  $ok && echo "self-test: ok"
}

[ "${1:-}" = --self-test ] && { self_test; exit; }
[ $# -ge 2 ] || usage
VERSION=$1 OUT=$2; shift 2
# The script changes directory (to the consumer module it builds), so OUT is made absolute now: the
# caller's relative path names a file in the caller's directory.
[ "$OUT" != - ] || die "OUT must be a file name; the SBOM is not written to standard output"
case "$OUT" in /*) ;; *) OUT=$PWD/$OUT ;; esac
WAIT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --wait) [ $# -ge 2 ] || usage; WAIT=$2; shift ;;
    *) usage ;;
  esac
  shift
done
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] || die "$VERSION is not a release version like v0.17.0 or v0.17.0-rc.1"
[[ "$WAIT" =~ ^[0-9]+$ ]] || die "--wait takes a number of minutes"
command -v syft >/dev/null || die "syft is not on the path"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bide-module-sbom.XXXXXX")
# Absolute, like OUT: a relative TMPDIR would break every $WORK path after the cd.
WORK=$(cd "$WORK" && pwd)
# The module cache is read-only (a downloaded toolchain even under -modcacherw).
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
mkdir -p "$WORK/consumer" "$WORK/empty"
# Resolved the way a user resolves it: only the public proxy and checksum database (named, so an
# environment that sets GOSUMDB=off or another database does not change it), a fresh cache.
gorun() { env GOWORK=off GOFLAGS="-mod=mod -modcacherw" GOMODCACHE="$WORK/modcache" GOPROXY="$PROXY" GOSUMDB=sum.golang.org GOPRIVATE= GONOPROXY= GONOSUMDB= GOINSECURE= go "$@"; }

published=$("$here/release.sh" --list-published)
commands=$("$here/release.sh" --list-commands)
modules="$ROOT_MODULE"
for m in $published; do modules="$modules $ROOT_MODULE/$m"; done

# Wait for the proxy to serve every module (the nested tags follow the root tag by minutes). Each tag
# is first checked without the proxy and given time to settle before the proxy is asked, then the
# proxy is asked with growing waits (scripts/proxy-wait.sh): asking sooner or more often makes it
# cache a miss for 30 to 60 minutes, which is what made the release wait far longer than the tags
# took. --wait bounds the whole wait.
# shellcheck source=scripts/proxy-wait.sh
. "$here/proxy-wait.sh"
pw_direct() { (cd "$WORK/empty" && env GOWORK=off GOFLAGS=-mod=mod GOMODCACHE="$WORK/directcache" GOPROXY=direct GOSUMDB=off GOPRIVATE= GONOPROXY= GONOSUMDB= GOINSECURE= go list -m "$1"); }
pw_proxy() { (cd "$WORK/empty" && gorun list -m "$1"); }
mkdir -p "$WORK/directcache"
start=$(date +%s)
for mod in $modules; do
  left=$(( WAIT * 60 - ($(date +%s) - start) ))
  [ "$left" -gt 0 ] || left=0
  # One deadline for the whole wait; a run never asks the proxy within the settle delay of its own start.
  # --wait 0 asks once and does not wait: no settle delay either.
  settle=${PW_SETTLE:-300}; [ "$WAIT" -gt 0 ] || settle=0
  if ! PW_SETTLE=$settle PW_FLOOR=$start PW_DEADLINE=$(( start + WAIT * 60 )) PW_DIRECT_MAX=$left PW_MAX=$left proxy_wait_resolve "$mod" "$VERSION"; then
    (cd "$WORK/empty" && gorun list -m "$mod@$VERSION") || true
    die "$mod@$VERSION does not resolve"
  fi
done

C=$WORK/consumer
cd "$C"
printf 'module example.com/bide-modules\n\ngo 1.27.0\n' > go.mod
"$here/release.sh" --consumer-main > main.go
libs=""
for m in $published; do
  case " $commands " in *" $m "*) ;; *) libs="$libs $ROOT_MODULE/$m@$VERSION" ;; esac
done
# shellcheck disable=SC2086
gorun get "$ROOT_MODULE@$VERSION" $libs >/dev/null 2>"$WORK/get.log" || { cat "$WORK/get.log" >&2; die "go get failed"; }
for m in $commands; do
  gorun get -tool "$ROOT_MODULE/$m@$VERSION" >/dev/null 2>>"$WORK/get.log" || { cat "$WORK/get.log" >&2; die "go get -tool $m failed"; }
done
gorun mod tidy
selected=$(gorun list -m -f '{{.Path}} {{.Version}}' all | grep "^${ROOT_MODULE}[ /]" || true)
wrong=$(echo "$selected" | awk -v v="$VERSION" 'NF && $2 != v')
[ -z "$wrong" ] || die "the consumer selected other versions of bide modules: $wrong"
for mod in $modules; do
  echo "$selected" | grep -qx "$mod $VERSION" || die "the consumer does not select $mod $VERSION"
done

# Only the module files: main.go holds no dependency of its own, and the cache is outside the tree.
# Licenses come from the modules this run downloaded, not from whatever cache the machine has.
GOMODCACHE="$WORK/modcache" SYFT_GOLANG_SEARCH_LOCAL_MOD_CACHE_LICENSES=true SYFT_GOLANG_SEARCH_REMOTE_LICENSES=false \
  syft scan "dir:$C" --source-name "bide-modules" --source-version "$VERSION" \
  --override-default-catalogers go-module-file-cataloger -o "spdx-json=$OUT" -q
# Every published module, and nothing else of bide's, at exactly vX.Y.Z.
listed=$(jq -r '.packages[] | select(.name == "'"$ROOT_MODULE"'" or (.name | startswith("'"$ROOT_MODULE"'/"))) | "\(.name) \(.versionInfo)"' "$OUT" | LC_ALL=C sort)
expected=$(for mod in $modules; do echo "$mod $VERSION"; done | LC_ALL=C sort)
[ "$listed" = "$expected" ] || die "the SBOM's bide modules are not the published ones at $VERSION: $(diff <(echo "$expected") <(echo "$listed") | grep '^[<>]' | tr '\n' ' ')"
want=$(echo "$modules" | wc -w | tr -d ' ')
echo "module-sbom: $OUT: $(jq '.packages | length' "$OUT") packages, the $want bide modules at $VERSION"
