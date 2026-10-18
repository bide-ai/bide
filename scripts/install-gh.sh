#!/usr/bin/env bash
# install-gh.sh: install a pinned GitHub CLI on a Linux x86-64 runner, checking the download against
# a SHA-256 kept here, and put it first on the PATH of the later steps of the job.
#
#   scripts/install-gh.sh               (CI: install into $RUNNER_TEMP and append to $GITHUB_PATH)
#   scripts/install-gh.sh --self-test   (CI: a download whose checksum differs is refused)
#
# The release workflow verifies build provenance with `gh attestation verify` before it signs or
# publishes anything, so the verifier is pinned like every other tool of the release: not whatever gh
# the runner image carries today. To move to another version, change GH_VERSION and GH_SHA256 together
# (the checksum is in the cli/cli release's gh_<version>_checksums.txt for gh_<version>_linux_amd64.tar.gz).
set -euo pipefail

GH_VERSION=${BIDE_GH_VERSION:-2.102.0}
GH_SHA256=${BIDE_GH_SHA256:-bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386}
GH_URL=${BIDE_GH_URL:-https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz}

die() { echo "install-gh: error: $*" >&2; exit 1; }

# install DEST: download, verify, unpack gh into DEST/bin.
install_gh() {
  local dest=$1 tmp
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  curl -fsSL --retry 3 -o "$tmp/gh.tar.gz" "$GH_URL" || die "could not download $GH_URL"
  local got; got=$(sha256sum "$tmp/gh.tar.gz" | cut -d' ' -f1)
  [ "$got" = "$GH_SHA256" ] || die "checksum of the download is $got, want $GH_SHA256"
  mkdir -p "$dest"
  tar -xzf "$tmp/gh.tar.gz" -C "$dest" --strip-components=1
  "$dest/bin/gh" --version | head -n 1 | grep -qF "gh version $GH_VERSION" || die "the installed gh is not $GH_VERSION"
}

self_test() {
  local tmp ok=true; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/pkg/gh_9.9.9_linux_amd64/bin"
  printf '#!/bin/sh\necho "gh version 9.9.9 (test)"\n' > "$tmp/pkg/gh_9.9.9_linux_amd64/bin/gh"
  chmod +x "$tmp/pkg/gh_9.9.9_linux_amd64/bin/gh"
  tar -czf "$tmp/gh.tar.gz" -C "$tmp/pkg" gh_9.9.9_linux_amd64
  local sum; sum=$(sha256sum "$tmp/gh.tar.gz" | cut -d' ' -f1)
  export BIDE_GH_VERSION=9.9.9 BIDE_GH_URL="file://$tmp/gh.tar.gz"
  if ! BIDE_GH_SHA256=$sum "$0" "$tmp/ok" >/dev/null 2>&1 || [ ! -x "$tmp/ok/bin/gh" ]; then
    echo "self-test: a download that matches its checksum was refused"; ok=false; fi
  if BIDE_GH_SHA256=0000000000000000000000000000000000000000000000000000000000000000 "$0" "$tmp/bad" >/dev/null 2>&1; then
    echo "self-test: a download with another checksum was accepted"; ok=false; fi
  [ ! -e "$tmp/bad/bin/gh" ] || { echo "self-test: a refused download was still unpacked"; ok=false; }
  if BIDE_GH_VERSION=8.8.8 BIDE_GH_SHA256=$sum "$0" "$tmp/wrong" >/dev/null 2>&1; then
    echo "self-test: a gh of another version was accepted"; ok=false; fi
  $ok && echo "self-test: ok"
  $ok
}

case "${1:-}" in
  --self-test) self_test ;;
  "")
    [ -n "${RUNNER_TEMP:-}" ] && [ -n "${GITHUB_PATH:-}" ] || die "run it on a GitHub Actions runner (RUNNER_TEMP and GITHUB_PATH are not set)"
    install_gh "$RUNNER_TEMP/gh"
    echo "$RUNNER_TEMP/gh/bin" >> "$GITHUB_PATH"
    ;;
  *) # self-test calls: install into the directory given
    install_gh "$1" ;;
esac
