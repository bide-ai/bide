#!/usr/bin/env bash
# repro.sh: rebuild a bide release's binaries from its tag and check they are byte-for-byte the ones
# the release published.
#
#   scripts/repro.sh vX.Y.Z [--from DIR]   (DIR holds the release's archives and checksums.txt;
#                                           default: download them with gh)
#   scripts/repro.sh --self-test            (CI: two builds of cmd/bide from copies of the tree at
#                                           different paths are identical)
#
# A release binary is built by goreleaser (.goreleaser.yaml) in cmd/bide at the root tag, with
# CGO_ENABLED=0, GOWORK=off, -trimpath, -buildvcs=false and -ldflags "-s -w -X main.version=TAG".
# This script checks the archives against checksums.txt, clones the tag, and for each archive
# builds the same target the same way with the Go version the released binary records (go version
# reads it from the binary), then compares the two binaries. Trusting a release binary then reduces
# to trusting the source at the tag and the Go toolchain: anyone can run this and get the same bytes.
# The archives themselves are not compared (their metadata is goreleaser's); their checksums are.
set -euo pipefail

REPO=${BIDE_GITHUB_REPO:-bide-ai/bide}

die() { echo "repro: error: $*" >&2; exit 1; }

# ldflags is the linker flag set the release uses; .goreleaser.yaml must say the same (checked by
# --self-test and by the release workflow before it publishes).
ldflags() { echo "-s -w -X main.version=$1"; }

# build src goos goarch version out [toolchain]: build cmd/bide in the tree src as the release does,
# with the Go toolchain named (go1.27.0, say; the go command fetches it) or else the default one.
build() {
  local src=$1 goos=$2 goarch=$3 version=$4 out=$5 toolchain=${6:-${GOTOOLCHAIN:-auto}}
  (cd "$src/cmd/bide" && env CGO_ENABLED=0 GOWORK=off GOFLAGS= GOTOOLCHAIN="$toolchain" GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -buildvcs=false -ldflags "$(ldflags "$version")" -o "$out" .)
}

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# goreleaser_ldflags root: the -ldflags line .goreleaser.yaml uses, with the tag template.
goreleaser_ldflags() { awk '/^    ldflags:/ {f=1; next} f && /^      - / {sub(/^      - /, ""); print; exit}' "$1/.goreleaser.yaml"; }

self_test() {
  local root tmp ok=true
  root=$(git rev-parse --show-toplevel)
  [ "$(goreleaser_ldflags "$root")" = "$(ldflags '{{ .Tag }}')" ] ||
    { echo "self-test: .goreleaser.yaml ldflags [$(goreleaser_ldflags "$root")] differ from repro.sh's [$(ldflags '{{ .Tag }}')]"; ok=false; }
  grep -q -- '- -trimpath' "$root/.goreleaser.yaml" && grep -q -- '- -buildvcs=false' "$root/.goreleaser.yaml" ||
    { echo "self-test: .goreleaser.yaml does not build with -trimpath and -buildvcs=false"; ok=false; }
  tmp=$(mktemp -d)
  # Two copies of the tree at paths of different lengths: -trimpath must remove them both.
  mkdir -p "$tmp/a" "$tmp/longer/path/b"
  git -C "$root" archive HEAD | tar -x -C "$tmp/a"
  git -C "$root" archive HEAD | tar -x -C "$tmp/longer/path/b"
  for t in linux/amd64 darwin/arm64 windows/amd64; do
    build "$tmp/a" "${t%/*}" "${t#*/}" v0.0.0-selftest "$tmp/a.bin"
    build "$tmp/longer/path/b" "${t%/*}" "${t#*/}" v0.0.0-selftest "$tmp/b.bin"
    if cmp -s "$tmp/a.bin" "$tmp/b.bin"; then echo "self-test: $t: two builds are identical ($(sha "$tmp/a.bin"))"
    else echo "self-test: $t: two builds from the same source differ"; ok=false; fi
  done
  rm -rf "$tmp"
  $ok && echo "self-test: ok"
  $ok
}

case "${1:-}" in
  --self-test) self_test; exit ;;
  ""|-h|--help) sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac

TAG=$1; shift
FROM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --from) FROM=$2; shift ;;
    *) die "unknown argument $1" ;;
  esac
  shift
done
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]] || die "$TAG is not a release tag like v0.14.0"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bide-repro.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
if [ -z "$FROM" ]; then
  FROM=$WORK/dl
  gh release download "$TAG" -R "$REPO" -p 'bide_*' -p checksums.txt -D "$FROM" || die "could not download the $TAG release"
fi
(cd "$FROM" && for a in bide_*; do
  want=$(awk -v f="$a" '$2 == f {print $1}' checksums.txt)
  [ -n "$want" ] || die "$a is not in checksums.txt"
  [ "$(sha "$a")" = "$want" ] || die "$a does not match checksums.txt"
done)
echo "==> the archives match checksums.txt"

git clone --quiet --depth 1 --branch "$TAG" "https://github.com/$REPO" "$WORK/src" 2>/dev/null ||
  git clone --quiet --depth 1 --branch "$TAG" "$(git rev-parse --show-toplevel)" "$WORK/src" || die "could not clone $TAG"
version=${TAG}
fail=false
for a in "$FROM"/bide_*; do
  name=$(basename "$a"); base=${name%.tar.gz}; base=${base%.zip}
  goos=$(echo "$base" | awk -F_ '{print $(NF-1)}'); goarch=$(echo "$base" | awk -F_ '{print $NF}')
  mkdir -p "$WORK/x/$base"
  case "$name" in
    *.tar.gz) tar -xzf "$a" -C "$WORK/x/$base" ;;
    *.zip) unzip -q "$a" -d "$WORK/x/$base" ;;
    *) continue ;;
  esac
  bin=$WORK/x/$base/bide; [ "$goos" = windows ] && bin=$bin.exe
  [ -f "$bin" ] || die "$name holds no bide binary"
  gv=$(go version "$bin" | awk '{print $2}')
  build "$WORK/src" "$goos" "$goarch" "$version" "$WORK/rebuilt" "$gv"
  if cmp -s "$bin" "$WORK/rebuilt"; then echo "    $goos/$goarch: identical ($gv, $(sha "$bin"))"
  else echo "    $goos/$goarch: DIFFERENT (released $(sha "$bin"), rebuilt $(sha "$WORK/rebuilt"), $gv)"; fail=true; fi
done
$fail && die "a rebuilt binary differs from the release"
echo "==> every $TAG binary rebuilds byte for byte from the tag"
