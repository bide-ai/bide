#!/usr/bin/env bash
# module-sbom.sh: the SBOM of bide's published Go modules at a release, as SPDX JSON.
#
#   scripts/module-sbom.sh vX.Y.Z OUT.json [--wait MINUTES]
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
# runs before scripts/release.sh has pushed the nested tags). It needs syft on the path.
set -euo pipefail

ROOT_MODULE=github.com/bide-ai/bide
PROXY=https://proxy.golang.org
here=$(cd "$(dirname "$0")" && pwd)

die() { echo "module-sbom: error: $*" >&2; exit 1; }
usage() { sed -n '2,4p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

[ $# -ge 2 ] || usage
VERSION=$1 OUT=$2; shift 2
WAIT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --wait) [ $# -ge 2 ] || usage; WAIT=$2; shift ;;
    *) usage ;;
  esac
  shift
done
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || die "$VERSION is not a release version like v0.17.0"
[[ "$WAIT" =~ ^[0-9]+$ ]] || die "--wait takes a number of minutes"
command -v syft >/dev/null || die "syft is not on the path"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bide-module-sbom.XXXXXX")
# The module cache is read-only (a downloaded toolchain even under -modcacherw).
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
mkdir -p "$WORK/consumer" "$WORK/empty"
# Resolved the way a user resolves it: only the public proxy and checksum database, a fresh cache.
gorun() { env GOWORK=off GOFLAGS="-mod=mod -modcacherw" GOMODCACHE="$WORK/modcache" GOPROXY="$PROXY" GOPRIVATE= GONOPROXY= GONOSUMDB= GOINSECURE= go "$@"; }

published=$("$here/release.sh" --list-published)
commands=$("$here/release.sh" --list-commands)
modules="$ROOT_MODULE"
for m in $published; do modules="$modules $ROOT_MODULE/$m"; done

# Wait for the proxy to serve every module (the nested tags follow the root tag by minutes).
deadline=$(( $(date +%s) + WAIT * 60 ))
for mod in $modules; do
  until (cd "$WORK/empty" && gorun list -m "$mod@$VERSION" >/dev/null 2>&1); do
    [ "$(date +%s)" -lt "$deadline" ] || { (cd "$WORK/empty" && gorun list -m "$mod@$VERSION") || true; die "$mod@$VERSION does not resolve"; }
    sleep 30
  done
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
