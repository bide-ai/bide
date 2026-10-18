#!/usr/bin/env bash
# verify-provenance.sh: check that each named file has SLSA Build L3 provenance made by bide's
# release builder for this repository, as a user would (docs/trust/verify-releases.md).
#
#   scripts/verify-provenance.sh [OPTIONS] TAG FILE...
#   scripts/verify-provenance.sh --self-test      (CI: the right policy is passed, a failure fails)
#
#   --workflow NAME       the builder workflow that made the files: build.yml (default; archives, their
#                         SBOMs, checksums.txt, approve-page.SHA256SUMS) or module-sbom.yml
#   --ref REF             the source ref the build ran for (default refs/tags/TAG; a manual run has a branch)
#   --source-digest SHA   also require this commit of bide
#   --builder-sha SHA     the commit of the builder that must have made the provenance (default: the
#                         one release.yml pins, read from that file; it must pin exactly one)
#
# Each file must verify against the attestations GitHub stores for the repository (BIDE_GITHUB_REPO,
# default bide-ai/bide): SLSA v1 provenance signed with the Sigstore public-good instance by the
# builder workflow (BIDE_BUILDER_REPO, default bide-ai/release-builder) at that commit, for this ref,
# from a GitHub-hosted runner. BIDE_VERIFY_ATTEMPTS (default 1) tries a file that many times, BIDE_VERIFY_SLEEP
# seconds apart (default 15): the attestations were stored moments before a release verifies them, and a
# lookup that races the store is retried; the last failure still fails the run. A file that does not verify fails the run; every file is still tried,
# so one run lists them all. It needs gh (the release pins one: scripts/install-gh.sh) and GH_TOKEN
# or a gh login.
set -euo pipefail

REPO=${BIDE_GITHUB_REPO:-bide-ai/bide}
BUILDER=${BIDE_BUILDER_REPO:-bide-ai/release-builder}
GH=${BIDE_GH:-gh}
ATTEMPTS=${BIDE_VERIFY_ATTEMPTS:-1}
SLEEP=${BIDE_VERIFY_SLEEP:-15}
RELEASE_YML=${BIDE_RELEASE_YML:-$(dirname "$0")/../.github/workflows/release.yml}

die() { echo "verify-provenance: error: $*" >&2; exit 1; }

# pinned_builder WORKFLOW: the commit release.yml pins for the builder workflow; exactly one.
pinned_builder() {
  local wf=$1 pins
  [ -f "$RELEASE_YML" ] || die "no $RELEASE_YML to read the builder pin from (use --builder-sha)"
  pins=$(grep -oE "$BUILDER/\.github/workflows/${wf//./\\.}@[0-9a-f]{40}" "$RELEASE_YML" | sed 's/.*@//' | sort -u || true)
  [ -n "$pins" ] || die "$RELEASE_YML does not pin $BUILDER/.github/workflows/$wf by commit"
  [ "$(wc -l <<<"$pins")" -eq 1 ] || die "$RELEASE_YML pins $wf at more than one commit: $(tr '\n' ' ' <<<"$pins")"
  echo "$pins"
}

verify() {
  local ref="" sha="" wf=build.yml bsha=""
  while [ $# -gt 0 ]; do
    case $1 in
      --ref|--source-digest|--workflow|--builder-sha)
        [ $# -ge 2 ] || die "$1 needs a value"
        case $1 in --ref) ref=$2 ;; --source-digest) sha=$2 ;; --workflow) wf=$2 ;; --builder-sha) bsha=$2 ;; esac
        shift 2 ;;
      *) break ;;
    esac
  done
  [ $# -ge 2 ] || die "usage: verify-provenance.sh [OPTIONS] TAG FILE..."
  local tag=$1; shift
  [ -n "$ref" ] || ref="refs/tags/$tag"
  [[ "$wf" =~ ^[a-z0-9-]+\.yml$ ]] || die "--workflow $wf is not a workflow file name"
  [ -n "$bsha" ] || bsha=$(pinned_builder "$wf")
  [[ "$bsha" =~ ^[0-9a-f]{40}$ ]] || die "builder commit '$bsha' is not a full commit SHA"
  local f bad=0
  for f in "$@"; do [ -f "$f" ] || die "no such file: $f"; done
  for f in "$@"; do
    local args=(attestation verify "$f" -R "$REPO"
      --signer-workflow "$BUILDER/.github/workflows/$wf"
      --signer-digest "$bsha"
      --source-ref "$ref"
      --predicate-type https://slsa.dev/provenance/v1
      --deny-self-hosted-runners)
    [ -z "$sha" ] || args+=(--source-digest "$sha")
    local n=0 done_=false
    while [ "$n" -lt "$ATTEMPTS" ]; do
      n=$((n + 1))
      if "$GH" "${args[@]}" >/dev/null; then done_=true; break; fi
      [ "$n" -ge "$ATTEMPTS" ] || { echo "provenance of $f did not verify (attempt $n of $ATTEMPTS); retrying" >&2; sleep "$SLEEP"; }
    done
    if $done_; then echo "provenance ok: $f"
    else echo "PROVENANCE DID NOT VERIFY: $f" >&2; bad=1; fi
  done
  [ "$bad" -eq 0 ] || die "at least one file has no valid provenance from $BUILDER/$wf@$bsha for $REPO at $ref"
}

self_test() {
  local tmp; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  local ok=true self=$0 a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa b=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  : > "$tmp/a.tar.gz"; : > "$tmp/checksums.txt"
  # A fake gh: logs its arguments, and fails for any file named in FAKE_GH_FAIL.
  cat > "$tmp/gh" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_GH_LOG"
case "$*" in *"${FAKE_GH_FAIL:-__none__}"*) exit 1 ;; esac
FAKE
  chmod +x "$tmp/gh"
  export BIDE_GH="$tmp/gh" FAKE_GH_LOG="$tmp/log"
  printf 'jobs:\n  build:\n    uses: bide-ai/release-builder/.github/workflows/build.yml@%s # v1.0.0\n  sbom:\n    uses: bide-ai/release-builder/.github/workflows/module-sbom.yml@%s # v1.0.0\n' "$a" "$b" > "$tmp/release.yml"
  export BIDE_RELEASE_YML="$tmp/release.yml"

  : > "$tmp/log"
  if ! "$self" --source-digest abc123 v1.2.3 "$tmp/a.tar.gz" "$tmp/checksums.txt" >/dev/null 2>&1; then
    echo "self-test: good attestations were refused"; ok=false; fi
  local want
  for want in "attestation verify $tmp/a.tar.gz" "attestation verify $tmp/checksums.txt" "-R bide-ai/bide" \
    "--signer-workflow bide-ai/release-builder/.github/workflows/build.yml" "--signer-digest $a" "--source-ref refs/tags/v1.2.3" \
    "--predicate-type https://slsa.dev/provenance/v1" "--deny-self-hosted-runners" "--source-digest abc123"; do
    grep -qF -- "$want" "$tmp/log" || { echo "self-test: gh was never called with [$want]"; ok=false; }
  done
  if grep -qF -- "--signer-repo" "$tmp/log"; then echo "self-test: --signer-repo passed beside --signer-workflow (gh refuses both)"; ok=false; fi

  # The module SBOM is checked against its own builder workflow and that workflow's pin.
  : > "$tmp/log"
  "$self" --workflow module-sbom.yml v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1 || { echo "self-test: module-sbom run refused"; ok=false; }
  grep -qF -- "--signer-workflow bide-ai/release-builder/.github/workflows/module-sbom.yml --signer-digest $b" "$tmp/log" ||
    { echo "self-test: the module SBOM was not checked against module-sbom.yml and its pin"; ok=false; }

  : > "$tmp/log"
  "$self" --ref refs/heads/main v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1 || { echo "self-test: --ref run refused"; ok=false; }
  grep -qF -- "--source-ref refs/heads/main" "$tmp/log" || { echo "self-test: --ref was not passed to gh"; ok=false; }
  if grep -qF -- "--source-digest" "$tmp/log"; then echo "self-test: --source-digest passed without being asked"; ok=false; fi

  : > "$tmp/log"
  "$self" --builder-sha "$b" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1 || { echo "self-test: --builder-sha run refused"; ok=false; }
  grep -qF -- "--signer-digest $b" "$tmp/log" || { echo "self-test: --builder-sha was not used"; ok=false; }

  # One bad file fails the run, and the good file after it is still tried.
  : > "$tmp/log"
  if FAKE_GH_FAIL="a.tar.gz" "$self" v1.2.3 "$tmp/a.tar.gz" "$tmp/checksums.txt" >"$tmp/out" 2>&1; then
    echo "self-test: a file whose attestation does not verify passed"; ok=false; fi
  grep -qF "PROVENANCE DID NOT VERIFY: $tmp/a.tar.gz" "$tmp/out" || { echo "self-test: the failing file was not named"; ok=false; }
  grep -qF "provenance ok: $tmp/checksums.txt" "$tmp/out" || { echo "self-test: later files were not tried after a failure"; ok=false; }

  # No files, a missing file, a bad workflow name and a bad commit are errors, not a vacuous pass.
  if "$self" v1.2.3 >/dev/null 2>&1; then echo "self-test: no files passed"; ok=false; fi
  if "$self" v1.2.3 "$tmp/missing" >/dev/null 2>&1; then echo "self-test: a missing file passed"; ok=false; fi
  if "$self" --workflow '../x.yml' v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then echo "self-test: a path as the workflow passed"; ok=false; fi
  if "$self" --builder-sha main v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then echo "self-test: a branch as the builder commit passed"; ok=false; fi

  # A lookup that fails once and then succeeds passes with attempts; one that always fails fails.
  cat > "$tmp/flaky" <<'FAKE'
#!/usr/bin/env bash
n=$(cat "$FAKE_GH_COUNT" 2>/dev/null || echo 0); echo $((n + 1)) > "$FAKE_GH_COUNT"
[ "$n" -ge "$FAKE_GH_OK_AFTER" ]
FAKE
  chmod +x "$tmp/flaky"
  echo 0 > "$tmp/count"
  if ! BIDE_GH="$tmp/flaky" FAKE_GH_COUNT="$tmp/count" FAKE_GH_OK_AFTER=1 BIDE_VERIFY_ATTEMPTS=3 BIDE_VERIFY_SLEEP=0 "$self" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then
    echo "self-test: a lookup that succeeds on the second attempt was refused"; ok=false; fi
  echo 0 > "$tmp/count"
  if BIDE_GH="$tmp/flaky" FAKE_GH_COUNT="$tmp/count" FAKE_GH_OK_AFTER=99 BIDE_VERIFY_ATTEMPTS=3 BIDE_VERIFY_SLEEP=0 "$self" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then
    echo "self-test: a lookup that never succeeds passed"; ok=false; fi
  [ "$(cat "$tmp/count")" = 3 ] || { echo "self-test: expected 3 attempts, got $(cat "$tmp/count")"; ok=false; }
  echo 0 > "$tmp/count"
  if BIDE_GH="$tmp/flaky" FAKE_GH_COUNT="$tmp/count" FAKE_GH_OK_AFTER=1 "$self" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then
    echo "self-test: with the default one attempt, a failing lookup passed"; ok=false; fi

  # The builder pin: absent, or two different commits for one workflow, is a failure.
  printf 'jobs:\n  build:\n    uses: actions/checkout@%s\n' "$a" > "$tmp/none.yml"
  if BIDE_RELEASE_YML="$tmp/none.yml" "$self" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then echo "self-test: a missing builder pin passed"; ok=false; fi
  printf 'uses: bide-ai/release-builder/.github/workflows/build.yml@%s # v1\nuses: bide-ai/release-builder/.github/workflows/build.yml@%s # v2\n' "$a" "$b" > "$tmp/two.yml"
  if BIDE_RELEASE_YML="$tmp/two.yml" "$self" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then echo "self-test: two builder pins passed"; ok=false; fi
  printf 'uses: bide-ai/release-builder/.github/workflows/build.yml@v1.0.0\n' > "$tmp/tag.yml"
  if BIDE_RELEASE_YML="$tmp/tag.yml" "$self" v1.2.3 "$tmp/a.tar.gz" >/dev/null 2>&1; then echo "self-test: a tag as the builder pin passed"; ok=false; fi
  $ok && echo "self-test: ok"
  $ok
}

case "${1:-}" in
  --self-test) self_test ;;
  *) verify "$@" ;;
esac
