#!/usr/bin/env bash
# verify-provenance.sh: check that each named file has a SLSA build provenance attestation made by
# bide's release workflow, as a user would (docs/trust/verify-releases.md).
#
#   scripts/verify-provenance.sh TAG FILE...              (verify with gh attestation verify)
#   scripts/verify-provenance.sh --source-digest SHA TAG FILE...   (also pin the commit)
#   scripts/verify-provenance.sh --ref REF TAG FILE...    (a ref other than refs/tags/TAG, for a manual run)
#   scripts/verify-provenance.sh --self-test              (CI: the right policy is passed, a failure fails)
#
# Each file must verify against the attestations GitHub stores for the repository (BIDE_GITHUB_REPO,
# default bide-ai/bide): the SLSA v1 provenance predicate, signed with the Sigstore public-good
# instance by the release.yml workflow of that repository at the tag, from a GitHub-hosted runner.
# The first file that does not verify fails the run; every file is still tried, so one run lists them
# all. It needs gh (2.67 or later) and GH_TOKEN or a gh login.
set -euo pipefail

REPO=${BIDE_GITHUB_REPO:-bide-ai/bide}
GH=${BIDE_GH:-gh}

die() { echo "verify-provenance: error: $*" >&2; exit 1; }

verify() {
  local ref="" sha=""
  while [ $# -gt 0 ]; do
    case $1 in
      --ref) [ $# -ge 2 ] || die "--ref needs a value"; ref=$2; shift 2 ;;
      --source-digest) [ $# -ge 2 ] || die "--source-digest needs a value"; sha=$2; shift 2 ;;
      *) break ;;
    esac
  done
  [ $# -ge 2 ] || die "usage: verify-provenance.sh [--ref REF] [--source-digest SHA] TAG FILE..."
  local tag=$1; shift
  [ -n "$ref" ] || ref="refs/tags/$tag"
  local f bad=0
  for f in "$@"; do [ -f "$f" ] || die "no such file: $f"; done
  for f in "$@"; do
    local args=(attestation verify "$f" -R "$REPO"
      --signer-workflow "$REPO/.github/workflows/release.yml"
      --source-ref "$ref"
      --predicate-type https://slsa.dev/provenance/v1
      --deny-self-hosted-runners)
    [ -z "$sha" ] || args+=(--source-digest "$sha")
    if "$GH" "${args[@]}" >/dev/null; then echo "provenance ok: $f"
    else echo "PROVENANCE DID NOT VERIFY: $f" >&2; bad=1; fi
  done
  [ "$bad" -eq 0 ] || die "at least one file has no valid provenance for $REPO at $ref"
}

self_test() {
  local tmp; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  local ok=true self=$0
  : > "$tmp/a.tar.gz"; : > "$tmp/checksums.txt"
  # A fake gh: logs its arguments, and fails for any file named in FAKE_GH_FAIL.
  cat > "$tmp/gh" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_GH_LOG"
case "$*" in *"${FAKE_GH_FAIL:-__none__}"*) exit 1 ;; esac
FAKE
  chmod +x "$tmp/gh"
  export BIDE_GH="$tmp/gh" FAKE_GH_LOG="$tmp/log"

  : > "$tmp/log"
  if ! "$self" --source-digest abc123 v1.2.3 "$tmp/a.tar.gz" "$tmp/checksums.txt" >/dev/null 2>&1; then
    echo "self-test: good attestations were refused"; ok=false; fi
  local want
  for want in "attestation verify $tmp/a.tar.gz" "attestation verify $tmp/checksums.txt" "-R bide-ai/bide" \
    "--signer-workflow bide-ai/bide/.github/workflows/release.yml" "--source-ref refs/tags/v1.2.3" \
    "--predicate-type https://slsa.dev/provenance/v1" "--deny-self-hosted-runners" "--source-digest abc123"; do
    grep -qF -- "$want" "$tmp/log" || { echo "self-test: gh was never called with [$want]"; ok=false; }
  done

  : > "$tmp/log"
  "$self" --ref refs/heads/main v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1 || { echo "self-test: --ref run refused"; ok=false; }
  grep -qF -- "--source-ref refs/heads/main" "$tmp/log" || { echo "self-test: --ref was not passed to gh"; ok=false; }
  if grep -qF -- "--source-digest" "$tmp/log"; then echo "self-test: --source-digest passed without being asked"; ok=false; fi

  # One bad file fails the run, and the good file after it is still tried.
  : > "$tmp/log"
  if FAKE_GH_FAIL="a.tar.gz" "$self" v1.2.3 "$tmp/a.tar.gz" "$tmp/checksums.txt" >"$tmp/out" 2>&1; then
    echo "self-test: a file whose attestation does not verify passed"; ok=false; fi
  grep -qF "PROVENANCE DID NOT VERIFY: $tmp/a.tar.gz" "$tmp/out" || { echo "self-test: the failing file was not named"; ok=false; }
  grep -qF "provenance ok: $tmp/checksums.txt" "$tmp/out" || { echo "self-test: later files were not tried after a failure"; ok=false; }

  # No files, and a missing file, are errors, not a vacuous pass.
  if "$self" v1.2.3 >/dev/null 2>&1; then echo "self-test: no files passed"; ok=false; fi
  if "$self" v1.2.3 "$tmp/missing" >/dev/null 2>&1; then echo "self-test: a missing file passed"; ok=false; fi
  $ok && echo "self-test: ok"
  $ok
}

case "${1:-}" in
  --self-test) self_test ;;
  *) verify "$@" ;;
esac
