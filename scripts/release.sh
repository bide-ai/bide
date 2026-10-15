#!/usr/bin/env bash
# release.sh: tag a bide release, the root module and every published nested module.
#
#   scripts/release.sh vX.Y.Z [--push] [--ref REF] [--remote NAME] [--no-test] [--keep]
#   scripts/release.sh --check-tag TAG     (CI: the module a nested tag names is releasable)
#   scripts/release.sh --check-classification
#   scripts/release.sh --list-published | --list-commands | --consumer-main   (scripts/module-sbom.sh)
#   scripts/release.sh --assemble-changelog vX.Y.Z YYYY-MM-DD   (release PR: fragments into CHANGELOG.md)
#   scripts/release.sh --self-test
#
# On main, every nested module requires github.com/bide-ai/bide v0.0.0 (and govern v0.0.0 for the
# governed-event logs) and resolves it through a replace directive, so the workspace and
# `GOWORK=off` builds use the code in the tree. A consumer cannot resolve v0.0.0, so a nested
# module is released from a commit made for the purpose:
#
#   1. The root module is tagged vX.Y.Z at REF (default HEAD, which must be origin/main with a green
#      CI run: main's push run for the commit, or a green merge queue run of the same tree, which
#      lets the preflight and a dry run go on while main's push run goes; a --push release then
#      waits for that push run (up to an hour) and tags only if it passed; see ci_status), the tag is pushed, and the script waits until the module proxy serves it.
#   2. In stages, each published module whose bide dependencies are already released gets a
#      go.mod that requires them at vX.Y.Z with their replace directives dropped, and a go.sum
#      resolved through the proxy (`go mod tidy`). The module is built, vetted and tested with
#      GOWORK=off against those published versions, one signed-off commit records the stage, each
#      module is tagged <dir>/vX.Y.Z at it, the tags are pushed at most three per push (GitHub
#      runs no workflow for the tags of a larger push), and the script waits for the proxy.
#      The first stage holds the modules that need only the root; the governed-event logs, which
#      also need govern, and the bide command, which needs the stores, come in the second.
#   3. Every published module is resolved with `go list -m <module>@vX.Y.Z`, and a scratch consumer
#      module that `go get`s all of them at vX.Y.Z builds with a fresh module cache. The released
#      bide command (go install cmd/bide@vX.Y.Z) runs bide init, and the project it writes builds
#      and passes its test against the released modules.
#
# The release commits are reachable from their tags only; main keeps its replace directives, so
# development is unchanged. Repo-only modules (examples, integration, benchmarks) are never tagged.
#
# A --push run that stopped (waiting for the proxy, say) is run again with the same arguments; it
# reuses the tags already on the remote. It resumes even when main has moved since the root tag was
# pushed (a pull request merged meanwhile), as long as the root tag points at the commit released.
#
# Nobody should `go get`, `go run` or `go list` a version before its tag is pushed: the module proxy
# caches the miss (for about 30 minutes), and the release then waits until the cached miss expires.
#
# Without --push nothing leaves the machine: the same steps run in a scratch clone, "pushing" to a
# scratch bare repository that stands in for GitHub, and the go command resolves the bide modules
# from it directly (GOPRIVATE, git url.insteadOf, a scratch module cache) instead of from the
# proxy. The dry run therefore exercises every tag, go.mod, go.sum, build, test and consumer
# check; only the preflight checks for main and CI report instead of failing.
set -euo pipefail

ROOT_MODULE=github.com/bide-ai/bide
GITHUB_REPO=${BIDE_GITHUB_REPO:-bide-ai/bide}
PROXY=https://proxy.golang.org

# Libraries users import. Each is tagged <dir>/vX.Y.Z at every release.
PUBLISHED="govern store/sqlite store/postgres mcptools trace codec/gcf govern/sqlitelog govern/redislog govern/postgreslog signer/pkcs11 signer/awskms signer/azurekv signer/gcpkms flowedit cmd/bide cmd/bide/bide-hsm cmd/bide-view"
# Published modules whose root package is a command (package main), installed with go install. The
# consumer check builds them instead of importing them, since a program cannot be imported.
# cmd/bide-view is a signpost to bide view, tagged one last time at v0.14.0 and then removed.
# cmd/bide/bide-hsm is bide approve for keys in an HSM or a KMS; it needs cgo, so it is released as
# source (go install) and not as a prebuilt binary (see .goreleaser.yaml). It lives under cmd/bide
# so that it can use that module's internal approvecmd package.
COMMANDS="cmd/bide cmd/bide/bide-hsm cmd/bide-view"
# Modules that exist only to keep their dependencies out of the core. They keep their replace
# directives and are never tagged.
REPO_ONLY="cmd/bide/bide-hsm/azuretest cmd/bide/bide-hsm/emulatortest signer/gcpkms/emulatortest signer/awskms/emulatortest examples/approval examples/govern examples/hsm-signing examples/mcp examples/observability examples/plan examples/stripe-refund integration benchmarks"

# gorun runs the go command the way the release resolves modules; the release flow below replaces it.
gorun() { env GOWORK=off go "$@"; }

die() { echo "release: error: $*" >&2; exit 1; }
say() { echo "==> $*"; }
note() { echo "    $*"; }

usage() { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

is_version() { [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; }

module_path() { # dir: the module path a nested directory holds
  echo "$ROOT_MODULE/$1"
}

in_list() { # word list...: whether word is one of list
  local w=$1; shift
  for x in "$@"; do [ "$x" = "$w" ] && return 0; done
  return 1
}

# modules_in root: every module directory under root, relative, "." for the root, sorted. It skips
# the directories the go tool skips (testdata, vendor, names starting with "." or "_"), as
# .github/scripts/check-modules.sh does.
modules_in() {
  (cd "$1" && find . -mindepth 1 -type d \( -name testdata -o -name vendor -o -name node_modules -o -name '.*' -o -name '_*' \) -prune \
    -o -name go.mod -type f -print) | sed -e 's|^\./||' -e 's|/\{0,1\}go\.mod$||' -e 's|^$|.|' | LC_ALL=C sort
}

# bide_requires dir: the bide modules (root or nested) that dir/go.mod requires, one per line.
bide_requires() {
  (cd "$1" && go mod edit -json) | jq -r --arg r "$ROOT_MODULE" \
    '.Require // [] | .[] | select(.Path == $r or (.Path | startswith($r + "/"))) | .Path'
}

# libraries: the published modules a consumer imports (every published module but the commands).
libraries() {
  local m
  for m in $PUBLISHED; do in_list "$m" $COMMANDS || echo "$m"; done
}

# consumer_main: the main.go of a consumer module that imports the core and every library. The
# release's last check builds it, and scripts/module-sbom.sh lists what it resolves.
consumer_main() {
  local m
  echo "// Command check imports every published bide module."
  echo "package main"
  echo
  echo "import ("
  echo "	_ \"$ROOT_MODULE/agent\""
  for m in $(libraries); do echo "	_ \"$(module_path "$m")\""; done
  echo ")"
  echo
  echo "func main() {}"
}

# check_classification root: every module in the tree is either published or repo-only, not both,
# and a published module requires no repo-only module.
check_classification() {
  local root=$1 ok=true m
  for m in $(modules_in "$root"); do
    [ "$m" = "." ] && continue
    if in_list "$m" $PUBLISHED; then
      in_list "$m" $REPO_ONLY && { echo "release: $m is both published and repo-only" >&2; ok=false; }
    elif ! in_list "$m" $REPO_ONLY; then
      echo "release: module $m is neither published nor repo-only; classify it in scripts/release.sh" >&2; ok=false
    fi
  done
  for m in $PUBLISHED $REPO_ONLY; do
    [ -f "$root/$m/go.mod" ] || { echo "release: $m is classified but $m/go.mod does not exist" >&2; ok=false; }
  done
  for m in $COMMANDS; do
    in_list "$m" $PUBLISHED || { echo "release: command $m is not published" >&2; ok=false; }
  done
  for m in $PUBLISHED; do
    [ -f "$root/$m/go.mod" ] || continue
    local dep
    for dep in $(bide_requires "$root/$m"); do
      [ "$dep" = "$ROOT_MODULE" ] && continue
      in_list "${dep#"$ROOT_MODULE"/}" $PUBLISHED || { echo "release: published $m requires $dep, which is not published" >&2; ok=false; }
    done
  done
  $ok
}

# check_gomod dir: dir/go.mod is releasable: no bide requirement at the v0.0.0 placeholder (or any
# other non-release version) and no replace directive for a bide module. It reads the file only.
check_gomod() {
  local dir=$1 json bad
  json=$(cd "$dir" && go mod edit -json)
  bad=$(echo "$json" | jq -r --arg r "$ROOT_MODULE" \
    '.Require // [] | .[] | select(.Path == $r or (.Path | startswith($r + "/"))) | select(.Version == "v0.0.0" or (.Version | test("^v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$") | not)) | "require \(.Path) \(.Version)"')
  bad+=$(echo "$json" | jq -r --arg r "$ROOT_MODULE" \
    '.Replace // [] | .[] | select(.Old.Path == $r or (.Old.Path | startswith($r + "/"))) | "\nreplace \(.Old.Path) => \(.New.Path)"')
  if [ -n "$bad" ]; then
    echo "release: $dir/go.mod is not releasable:" >&2
    echo "$bad" | sed '/^$/d; s/^/    /' >&2
    return 1
  fi
}

# check_tag tag: for a nested tag <dir>/vX.Y.Z in the current checkout, dir is a published module
# whose go.mod is releasable and which builds and vets with GOWORK=off against the versions it
# requires. The root tag needs no check here (release.yml builds it).
check_tag() {
  local tag=$1 dir version
  dir=${tag%/*}; version=${tag##*/}
  is_version "$version" || die "$tag: $version is not a semantic version"
  if [ "$dir" = "$tag" ]; then echo "release: $tag is a root tag; nothing to check"; return 0; fi
  in_list "$dir" $PUBLISHED || die "$tag: $dir is not a published module (published: $PUBLISHED)"
  [ -f "$dir/go.mod" ] || die "$tag: $dir/go.mod does not exist"
  [ "$(cd "$dir" && go mod edit -json | jq -r .Module.Path)" = "$(module_path "$dir")" ] || die "$tag: $dir/go.mod does not declare $(module_path "$dir")"
  check_gomod "$dir"
  (cd "$dir" && gorun build ./... && gorun vet ./...)
  echo "release: $tag: $dir requires released versions only and builds with GOWORK=off"
}

# changelog_has text version: the changelog text has a "## [version]" section heading.
# It reads the text from a variable, not a pipe, and compares literally.
changelog_has() {
  local line
  while IFS= read -r line; do
    [[ $line == "## [$2]"* ]] && return 0
  done <<<"$1"
  return 1
}

# assemble_changelog version date: fold the fragments in changes/unreleased into CHANGELOG.md as the
# section for the version, update its compare links, and delete the fragments (see
# internal/tools/changelog). Run it in the release pull request, which then shows the assembled
# section for review.
assemble_changelog() {
  is_version "$1" || die "$1 is not a version like v0.8.0"
  (cd "$(git rev-parse --show-toplevel)" && go run ./internal/tools/changelog assemble -version "${1#v}" -date "$2")
}

# remote_has_tag lsremote ref: the `git ls-remote --tags` output lists ref exactly.
# awk reads all of its input and compares the ref literally.
remote_has_tag() { awk -v t="$2" '$2 == t {found = 1} END {exit !found}' <<<"$1"; }

# main_check sha main root_remote: whether a release of sha may go on with $REMOTE/main at main,
# when the root tag is on the remote at root_remote (empty when it is not): "main" when sha is main;
# "resuming" when it is not but the root tag already points at sha (a resumed --push run, after
# main moved while the release waited for the proxy); "moved" otherwise.
main_check() {
  if [ "$1" = "$2" ]; then echo main
  elif [ -n "$3" ] && [ "$3" = "$1" ]; then echo resuming
  else echo moved
  fi
}

# ci_status REPO SHA TREE: whether CI passed for the release commit SHA, whose tree is TREE:
#   "success" every ci.yml push run of SHA on main has completed and passed (a run still going, a
#             manual re-run for one, is not a pass: see push_ci_state);
#   "queue"   no push run has completed and passed yet, none failed, and a ci.yml run of the merge queue (event
#             merge_group) succeeded on a commit with the same tree as SHA. The queue runs the matrix
#             on the batch before it lands on main, and the commit it creates on main has the tree
#             the queue tested. That lets the preflight and a dry run go on while main's push run
#             goes, but it is weaker than the push run (the queue reuses the test cache and skips the
#             Go jobs for a documentation-only batch; main's run starts from an empty test cache), so
#             the root tag is not pushed until the push run has passed (root_tag_gate);
#   "failure" a completed push run of SHA failed or was cancelled (this wins over a green queue run:
#             the same tree failed on main);
#   "none"    no completed push run of SHA, and no successful queue run of this tree;
#   "unknown" GitHub could not be queried.
# Trees are compared, not commit SHAs: the queue's commit is not the commit that lands on main.
# A queue run that failed does not count, however many; a different tree does not count either.
ci_status() {
  local repo=$1 sha=$2 tree=$3 push heads head t
  push=$(push_ci_state "$repo" "$sha")
  case "$push" in success|failure) echo "$push"; return ;; unknown) echo unknown; return ;; esac
  heads=$(gh run list -R "$repo" --workflow ci.yml --event merge_group --status success --limit 100 --json headSha --jq '.[].headSha' 2>/dev/null) || { echo unknown; return; }
  for head in $heads; do
    t=$(gh api "repos/$repo/git/commits/$head" --jq .tree.sha 2>/dev/null) || continue
    if [ "$t" = "$tree" ]; then echo "queue"; return; fi
  done
  echo none
}

# push_ci_state REPO SHA: the state of main's ci.yml push runs of SHA: "failure" when a completed one
# failed or was cancelled, "running" when one has not completed, "success" when all completed and
# passed, "none" when there is none yet, "unknown" when GitHub could not be queried.
push_ci_state() {
  gh run list -R "$1" --workflow ci.yml --commit "$2" --event push --json status,conclusion \
    --jq 'if length == 0 then "none" elif any(.[]; .status == "completed" and .conclusion != "success") then "failure" elif any(.[]; .status != "completed") then "running" else "success" end' 2>/dev/null || echo unknown
}

# root_tag_gate REPO SHA BASIS: run just before the root tag is pushed, which cannot be undone (the
# module proxy keeps it). BASIS is what the preflight found ("success" or "queue", see ci_status).
# It does not trust the preflight alone: it polls (the first poll answers at once when the push run
# has passed), so a push run started since, a manual re-run, is waited for, and one that then fails
# stops the release even though an earlier run passed. It polls every
# BIDE_CI_POLL_SECS (30) for up to BIDE_CI_WAIT_SECS (3600), for main's push run of SHA to complete,
# and passes only if it succeeded; a failed run, or one still going or missing when the time is up,
# stops the release before anything is tagged. Returns 0 to go on, 1 to stop.
root_tag_gate() {
  local repo=$1 sha=$2 basis=$3 poll=${BIDE_CI_POLL_SECS:-30} wait=${BIDE_CI_WAIT_SECS:-3600} n i st
  n=$(awk -v w="$wait" -v p="$poll" 'BEGIN { n = int(w / p); if (n < 1) n = 1; print n }')
  [ "$basis" != queue ] || note "the preflight rested on the merge queue's run of the same tree, which is weaker than main's push run" >&2
  note "checking main's CI push run of $sha before tagging (waits up to $((wait / 60)) min for one that is still going)" >&2
  for ((i = 1; i <= n; i++)); do
    st=$(push_ci_state "$repo" "$sha")
    case "$st" in
      success) note "main's CI push run of $sha passed" >&2; return 0 ;;
      failure) echo "release: main's CI push run of $sha failed; the merge queue's run passed, but the release stops before tagging" >&2; return 1 ;;
    esac
    [ "$i" -lt "$n" ] && sleep "$poll"
  done
  echo "release: main's CI push run of $sha is $st after $((wait / 60)) min; nothing was tagged (re-run the release when it has passed)" >&2
  return 1
}

# GitHub creates no push event, and so runs no workflow (Release modules, Release), for the tags
# of a push that carries more than three tags.
MAX_TAGS_PER_PUSH=3

# push_tags tag...: push the tags from $CLONE to $TARGET, at most MAX_TAGS_PER_PUSH per push, so
# each tag runs its workflows. A failed push stops the release; re-running it reuses the tags that
# reached the remote.
push_tags() {
  local batch=()
  while [ $# -gt 0 ]; do
    batch+=("$1"); shift
    if [ ${#batch[@]} -eq "$MAX_TAGS_PER_PUSH" ] || [ $# -eq 0 ]; then
      git -C "$CLONE" push --quiet "$TARGET" "${batch[@]/#/refs/tags/}" || die "pushing ${batch[*]} failed; re-run to resume"
      if $PUSH; then note "pushed ${batch[*]}"; else note "pushed to the scratch origin: ${batch[*]}"; fi
      batch=()
    fi
  done
}

# self_test_ci: ci_status against a fake gh that serves fixture runs (the real jq filters run on
# them): a green push run; a failed push run beside a green queue run of the same tree; a green
# queue run of the same tree and no push run; a green queue run of another tree; a failed queue run
# of the same tree; a queue commit the API cannot read; GitHub unreachable.
self_test_ci() {
  command -v jq >/dev/null 2>&1 || { echo "self-test: jq is not installed; skipping the CI status cases"; return 0; }
  local tmp ok=true want got; tmp=$(mktemp -d)
  mkdir -p "$tmp/bin" "$tmp/fx"
  cat > "$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
# A fake gh: "run list" serves $FX/push.json or $FX/mg.json (filtered by --status like gh), "api
# repos/R/git/commits/SHA" serves $FX/tree-SHA; $FX/down makes every call fail.
[ ! -e "$FX/down" ] || exit 1
expr= prev= status=
for a in "$@"; do
  [ "$prev" = --jq ] && expr=$a
  [ "$prev" = --status ] && status=$a
  prev=$a
done
case "$*" in
  *"--event push"*)
    f=$FX/push.json
    if [ -e "$FX/push.1.json" ]; then # a sequence: one file per call, the last repeats
      n=$(( $(cat "$FX/n" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FX/n"
      f=$FX/push.$n.json; [ -e "$f" ] || { n=$(ls "$FX"/push.[0-9]*.json | wc -l | tr -d ' '); f=$FX/push.$n.json; }
    fi ;;
  *"--event merge_group"*) f=$FX/mg.json ;;
  "api repos/"*"/git/commits/"*) sha=${2##*/}; [ -f "$FX/tree-$sha" ] || exit 1; cat "$FX/tree-$sha"; exit 0 ;;
  *) exit 1 ;;
esac
if [ -n "$status" ]; then jq -c --arg s "$status" 'map(select(.conclusion == $s))' "$f" | jq -r "$expr"; else jq -r "$expr" "$f"; fi
GH
  chmod +x "$tmp/bin/gh"
  local T=tree-of-release
  ci_case() { # ci_case WANT PUSH-JSON MERGE-GROUP-JSON DESCRIPTION [down]
    rm -f "$tmp/fx/down" "$tmp/fx"/tree-*
    printf '%s' "$2" > "$tmp/fx/push.json"; printf '%s' "$3" > "$tmp/fx/mg.json"
    printf '%s\n' "$T" > "$tmp/fx/tree-q-same"; printf 'tree-of-another\n' > "$tmp/fx/tree-q-other"
    [ -z "${5:-}" ] || : > "$tmp/fx/down"
    got=$(FX="$tmp/fx" PATH="$tmp/bin:$PATH" ci_status example/r release-sha "$T")
    [ "$got" = "$1" ] || { echo "self-test: $4: got $got, want $1"; ok=false; }
  }
  local pass='[{"status":"completed","conclusion":"success"}]' fail='[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"failure"}]'
  local q_same='[{"conclusion":"success","headSha":"q-same"}]' q_other='[{"conclusion":"success","headSha":"q-other"}]'
  local q_failed='[{"conclusion":"failure","headSha":"q-same"}]' q_unreadable='[{"conclusion":"success","headSha":"q-gone"},{"conclusion":"success","headSha":"q-same"}]'
  ci_case success "$pass" '[]' "a green push run"
  ci_case failure "$fail" "$q_same" "a failed push run beside a green queue run of the same tree"
  ci_case queue '[]' "$q_same" "a green queue run of the same tree, no push run"
  ci_case queue '[{"status":"in_progress","conclusion":""}]' "$q_same" "a green queue run of the same tree, the push run still going"
  ci_case none '[{"status":"completed","conclusion":"success"},{"status":"in_progress","conclusion":""}]' '[]' "a green push run and a re-run still going, no queue run"
  ci_case queue '[{"status":"completed","conclusion":"success"},{"status":"in_progress","conclusion":""}]' "$q_same" "a green push run and a re-run still going, a green queue run of the same tree"
  ci_case none '[]' "$q_other" "a green queue run of another tree"
  ci_case none '[]' "$q_failed" "a failed queue run of the same tree"
  ci_case none '[]' '[]' "no run at all"
  ci_case queue '[]' "$q_unreadable" "a queue commit the API cannot read before one that matches"
  ci_case unknown "$pass" "$q_same" "GitHub unreachable" down
  # root_tag_gate: the fake gh serves push.1.json, push.2.json, ... one per call (the last one repeats).
  gate_case() { # gate_case WANT BASIS DESCRIPTION PUSH-JSON...
    local want=$1 basis=$2 desc=$3 k=0 f
    shift 3
    rm -f "$tmp/fx/n" "$tmp/fx"/push.[0-9]*.json
    for f in "$@"; do k=$((k + 1)); printf '%s' "$f" > "$tmp/fx/push.$k.json"; done
    rm -f "$tmp/fx/down"
    got=$(FX="$tmp/fx" PATH="$tmp/bin:$PATH" BIDE_CI_POLL_SECS=0.1 BIDE_CI_WAIT_SECS=1 root_tag_gate example/r release-sha "$basis" >/dev/null 2>&1 && echo go || echo stop)
    [ "$got" = "$want" ] || { echo "self-test: $desc: the gate said $got, want $want"; ok=false; }
  }
  local running='[{"status":"in_progress","conclusion":""}]' pass_and_rerun='[{"status":"completed","conclusion":"success"},{"status":"in_progress","conclusion":""}]'
  gate_case go queue "the push run still going, then green: the root tag is pushed" "$running" "$running" "$pass"
  gate_case stop queue "the push run still going, then failed (a later green run does not undo it): stopped before tagging" "$running" "$fail" "$pass"
  gate_case stop queue "the push run never completes: stopped at the time limit" "$running"
  gate_case stop queue "no push run appears: stopped at the time limit" '[]'
  gate_case stop queue "the push run is already failed" "$fail"
  gate_case go queue "the push run already green" "$pass"
  gate_case go success "the push run is green and the preflight saw it" "$pass"
  gate_case stop success "a re-run of the push run started after a green one (the preflight saw green) and fails" "$pass_and_rerun" "$fail"
  gate_case go success "a re-run of the push run is going after a green one, then passes" "$pass_and_rerun" "$pass"
  gate_case stop queue "an earlier run passed, a re-run is still going at the time limit" "$pass_and_rerun"
  gate_case stop queue "a completed run failed while another is going: stops at once, not after the other passes" '[{"status":"completed","conclusion":"failure"},{"status":"in_progress","conclusion":""}]' "$pass"
  # The gate comes before the root tag is created and pushed (the self-test cannot run a release).
  local gate_line tag_line
  gate_line=$(grep -n 'root_tag_gate "\$GITHUB_REPO"' "${BASH_SOURCE[0]}" | head -1 | cut -d: -f1)
  tag_line=$(grep -n 'git -C "\$CLONE" tag -a "\$VERSION"' "${BASH_SOURCE[0]}" | head -1 | cut -d: -f1)
  { [ -n "$gate_line" ] && [ -n "$tag_line" ] && [ "$gate_line" -lt "$tag_line" ]; } || { echo "self-test: the root tag gate does not come before the root tag"; ok=false; }
  rm -rf "$tmp"
  $ok
}

# self_test_push: push_tags pushes ten tags (more than a release stage holds, and not a multiple of
# three) to a target that refuses any push of more than three tags, and every tag arrives.
self_test_push() {
  local tmp ok=true t want="" got g
  tmp=$(mktemp -d)
  g=(-c user.name=t -c user.email=t@example.com -c commit.gpgsign=false -c tag.gpgsign=false)
  git init --quiet "$tmp/src"
  git -C "$tmp/src" "${g[@]}" commit --quiet --allow-empty -m init
  for t in a b c d e f g h i j; do
    git -C "$tmp/src" "${g[@]}" tag -a "$t/v0.8.0" -m "$t"
    want="$want $t/v0.8.0"
  done
  git init --quiet --bare "$tmp/target.git"
  git -C "$tmp/target.git" config core.hooksPath hooks
  cat >"$tmp/target.git/hooks/pre-receive" <<HOOK
#!/usr/bin/env bash
n=\$(grep -c 'refs/tags/' || true)
echo "\$n" >>"$tmp/pushes.log"
[ "\$n" -le 3 ] || { echo "pre-receive: \$n tags in one push; GitHub runs no workflow for them" >&2; exit 1; }
HOOK
  chmod +x "$tmp/target.git/hooks/pre-receive"
  # shellcheck disable=SC2086
  (CLONE=$tmp/src TARGET=$tmp/target.git PUSH=false; push_tags $want >/dev/null 2>"$tmp/push.err") ||
    { echo "self-test: pushing ten tags failed:"; sed 's/^/    /' "$tmp/push.err"; ok=false; }
  if [ -f "$tmp/pushes.log" ] && awk '$1 > 3 {bad = 1} END {exit bad}' "$tmp/pushes.log"; then :; else
    echo "self-test: a push carried more than three tags: $(tr '\n' ' ' <"$tmp/pushes.log" 2>/dev/null)"; ok=false
  fi
  got=$(git -C "$tmp/target.git" tag -l | LC_ALL=C sort | tr '\n' ' ')
  [ "$got" = "$(echo $want | tr ' ' '\n' | LC_ALL=C sort | tr '\n' ' ')" ] ||
    { echo "self-test: the target has tags [$got], want [$want ]"; ok=false; }
  rm -rf "$tmp"
  $ok
}

# self_test_changelog: the changelog tool folds two fragments into a scratch repository's
# CHANGELOG.md as a new section, moves the compare links, and deletes the fragments.
self_test_changelog() {
  local tmp top ok=true; tmp=$(mktemp -d); top=$(git rev-parse --show-toplevel)
  mkdir -p "$tmp/internal/tools" "$tmp/changes/unreleased"
  cp -R "$top/internal/tools/changelog" "$tmp/internal/tools/"
  printf 'module example.com/s\n\ngo 1.27\n' >"$tmp/go.mod"
  printf '# Changelog\n\n## [Unreleased]\n\nUnreleased changes are in `changes/unreleased/`.\n\n## [0.8.0] - 2026-09-30\n\n### Added\n\n- old\n\n[Unreleased]: https://example.com/r/compare/v0.8.0...HEAD\n[0.8.0]: https://example.com/r/compare/v0.7.0...v0.8.0\n' >"$tmp/CHANGELOG.md"
  printf 'Section: Fixed\n\nfix one\n' >"$tmp/changes/unreleased/b.md"
  printf 'Section: Added\n\nadd one\n' >"$tmp/changes/unreleased/a.md"
  if ! (cd "$tmp" && GOWORK=off go run ./internal/tools/changelog assemble -version 0.9.0 -date 2026-10-01 >/dev/null 2>"$tmp/err"); then
    echo "self-test: assembling the changelog failed:"; sed 's/^/    /' "$tmp/err"; ok=false
  else
    grep -q '^## \[0.9.0\] - 2026-10-01$' "$tmp/CHANGELOG.md" || { echo "self-test: the assembled changelog has no [0.9.0] section"; ok=false; }
    [ "$(grep -e '^### Added$' -e '^### Fixed$' "$tmp/CHANGELOG.md" | head -2 | tr '\n' ' ')" = "### Added ### Fixed " ] || { echo "self-test: the assembled sections are not in Added, Fixed order"; ok=false; }
    grep -q '^\[Unreleased\]: https://example.com/r/compare/v0.9.0...HEAD$' "$tmp/CHANGELOG.md" || { echo "self-test: the [Unreleased] link did not move to v0.9.0"; ok=false; }
    grep -q '^\[0.9.0\]: https://example.com/r/compare/v0.8.0...v0.9.0$' "$tmp/CHANGELOG.md" || { echo "self-test: the [0.9.0] compare link is missing"; ok=false; }
    [ -z "$(ls "$tmp/changes/unreleased" 2>/dev/null)" ] || { echo "self-test: fragments are left after assembling"; ok=false; }
  fi
  rm -rf "$tmp"
  $ok
}

self_test() {
  local tmp fail=false; tmp=$(mktemp -d)
  printf 'module %s/x\n\ngo 1.27\n\nrequire %s v0.0.0\n\nreplace %s => ../\n' "$ROOT_MODULE" "$ROOT_MODULE" "$ROOT_MODULE" > "$tmp/go.mod"
  check_gomod "$tmp" 2>/dev/null && { echo "self-test: v0.0.0 with replace was accepted"; fail=true; }
  printf 'module %s/x\n\ngo 1.27\n\nrequire %s v0.0.0\n' "$ROOT_MODULE" "$ROOT_MODULE" > "$tmp/go.mod"
  check_gomod "$tmp" 2>/dev/null && { echo "self-test: v0.0.0 without replace was accepted"; fail=true; }
  printf 'module %s/x\n\ngo 1.27\n\nrequire (\n\t%s v0.8.0\n\t%s/govern v0.0.0\n)\n' "$ROOT_MODULE" "$ROOT_MODULE" "$ROOT_MODULE" > "$tmp/go.mod"
  check_gomod "$tmp" 2>/dev/null && { echo "self-test: govern v0.0.0 was accepted"; fail=true; }
  printf 'module %s/x\n\ngo 1.27\n\nrequire %s v0.8.0\n\nreplace %s => ../\n' "$ROOT_MODULE" "$ROOT_MODULE" "$ROOT_MODULE" > "$tmp/go.mod"
  check_gomod "$tmp" 2>/dev/null && { echo "self-test: a released version with a replace was accepted"; fail=true; }
  printf 'module %s/x\n\ngo 1.27\n\nrequire (\n\t%s v0.8.0\n\t%s/govern v0.8.0\n\texample.com/other v0.0.0\n)\n' "$ROOT_MODULE" "$ROOT_MODULE" "$ROOT_MODULE" > "$tmp/go.mod"
  check_gomod "$tmp" || { echo "self-test: released versions were refused"; fail=true; }
  rm -rf "$tmp"
  # The preflight checks read their whole input: under pipefail, a reader that stops at its first
  # match makes the writer fail with SIGPIPE (141), which read as "not found". The inputs are
  # larger than a pipe buffer and match on their first line.
  local filler changelog tags
  filler=$(printf -- '- entry %s\n' $(seq 1 20000))
  changelog=$(printf '# Changelog\n\n## [Unreleased]\n\n## [0.8.0] - 2026-09-30\n\n%s\n' "$filler")
  changelog_has "$changelog" 0.8.0 || { echo "self-test: a changelog's [0.8.0] section was not found"; fail=true; }
  changelog_has "$changelog" 0.9.0 && { echo "self-test: a changelog's missing [0.9.0] section was found"; fail=true; }
  changelog_has "$changelog" 0.8.1 && { echo "self-test: [0.8.0] was taken for [0.8.1]"; fail=true; }
  tags=$(printf '%040d\trefs/tags/govern/v0.8.0\n' 0; for i in $(seq 1 20000); do printf '%040d\trefs/tags/x%s/v0.1.0\n' "$i" "$i"; done)
  remote_has_tag "$tags" refs/tags/govern/v0.8.0 || { echo "self-test: a remote tag was not found"; fail=true; }
  remote_has_tag "$(printf '%040d\trefs/tags/govern/v0x8x0\n' 0)" refs/tags/govern/v0.8.0 && { echo "self-test: govern/v0x8x0 was taken for govern/v0.8.0"; fail=true; }
  remote_has_tag "$tags" refs/tags/mcp/v0.8.0 && { echo "self-test: a missing remote tag was found"; fail=true; }
  # A release resumed after main moved goes on when the root tag already points at its commit.
  local a b; a=$(printf '%040d' 1); b=$(printf '%040d' 2)
  [ "$(main_check "$a" "$a" "")" = main ] || { echo "self-test: a release of main was refused"; fail=true; }
  [ "$(main_check "$a" "$b" "$a")" = resuming ] || { echo "self-test: a release whose root tag is on the remote at its commit did not resume after main moved"; fail=true; }
  [ "$(main_check "$a" "$b" "")" = moved ] || { echo "self-test: a release of a commit that is not main, with no root tag, was not refused"; fail=true; }
  [ "$(main_check "$a" "$b" "$b")" = moved ] || { echo "self-test: a root tag at another commit let a release that is not main go on"; fail=true; }
  # GitHub creates no push event, so runs no workflow, for the tags of a push that carries more
  # than three. The scratch target's pre-receive hook refuses such a push and logs each push's
  # size; every tag must still arrive.
  # A command is published but not imported: the consumer imports every other published module.
  local libs
  libs=$(PUBLISHED="a cmd/x b" COMMANDS="cmd/x" libraries | tr '\n' ' ')
  [ "$libs" = "a b " ] || { echo "self-test: the consumer would import [$libs], want [a b ]"; fail=true; }
  in_list cmd/bide $(libraries) && { echo "self-test: the consumer would import the bide command"; fail=true; }
  in_list cmd/bide-view $(libraries) && { echo "self-test: the consumer would import the bide-view signpost"; fail=true; }
  in_list cmd/bide/bide-hsm $(libraries) && { echo "self-test: the consumer would import the bide-hsm command"; fail=true; }
  in_list cmd/bide/bide-hsm $PUBLISHED || { echo "self-test: bide-hsm is not published"; fail=true; }
  # The consumer (the release's last check and the module SBOM) imports the core and every library,
  # and no command; the module SBOM lists the commands as tools.
  local main
  main=$(PUBLISHED="a cmd/x b" COMMANDS="cmd/x" consumer_main)
  printf '%s\n' "$main" | grep -qx "	_ \"$ROOT_MODULE/agent\"" || { echo "self-test: the consumer does not import the core"; fail=true; }
  printf '%s\n' "$main" | grep -qx "	_ \"$ROOT_MODULE/a\"" || { echo "self-test: the consumer does not import library a"; fail=true; }
  printf '%s\n' "$main" | grep -qx "	_ \"$ROOT_MODULE/b\"" || { echo "self-test: the consumer does not import library b"; fail=true; }
  printf '%s\n' "$main" | grep -q "cmd/x" && { echo "self-test: the consumer imports a command"; fail=true; }
  printf '%s\n' "$main" | grep -qx 'package main' || { echo "self-test: the consumer is not a main package"; fail=true; }
  [ "$(PUBLISHED="a cmd/x b" COMMANDS="cmd/x" "$0" --list-commands 2>/dev/null | tr '\n' ' ')" = "cmd/bide cmd/bide/bide-hsm cmd/bide-view " ] ||
    { echo "self-test: --list-commands does not print the commands"; fail=true; }
  self_test_push || fail=true
  self_test_ci || fail=true
  self_test_changelog || fail=true
  $fail && return 1
  echo "self-test: ok"
}

case "${1:-}" in
  --check-tag) [ $# -eq 2 ] || usage; check_tag "$2"; exit ;;
  --check-classification) check_classification "$(git rev-parse --show-toplevel)" && echo "release: every module is classified"; exit ;;
  --list-published) for m in $PUBLISHED; do echo "$m"; done; exit ;;
  --list-commands) for m in $COMMANDS; do echo "$m"; done; exit ;;
  --consumer-main) consumer_main; exit ;;
  --self-test) self_test; exit ;;
  --assemble-changelog) [ $# -eq 3 ] || usage; assemble_changelog "$2" "$3"; exit ;;
  ""|-h|--help) usage ;;
esac

VERSION=$1; shift
is_version "$VERSION" || die "$VERSION is not a version like v0.8.0"
PUSH=false REF=HEAD REMOTE=origin TEST=true KEEP=false
while [ $# -gt 0 ]; do
  case "$1" in
    --push) PUSH=true ;;
    --ref) REF=$2; shift ;;
    --remote) REMOTE=$2; shift ;;
    --no-test) TEST=false ;;
    --keep) KEEP=true ;;
    *) usage ;;
  esac
  shift
done

REPO=$(git rev-parse --show-toplevel)
SHA=$(git -C "$REPO" rev-parse --verify "$REF^{commit}")
WORK=$(mktemp -d "${TMPDIR:-/tmp}/bide-release.XXXXXX")
if $KEEP || $PUSH; then
  trap 'echo "release: work tree kept at $WORK"' EXIT
else
  trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
fi

if $PUSH; then say "release $VERSION from $SHA (PUSH: tags go to $REMOTE)"; else say "release $VERSION from $SHA (dry run: nothing is pushed)"; fi

# problem msg: a failed preflight check stops a real release and is reported by a dry run.
problem() {
  if $PUSH; then die "$*"; fi
  echo "    DRY RUN WARNING: $* (a --push release stops here)"
}

# ---- preflight ------------------------------------------------------------------------------
say "preflight"
check_classification "$REPO" || die "module classification is out of date"
note "published: $PUBLISHED"
note "repo-only: $REPO_ONLY"
[ -z "$(git -C "$REPO" status --porcelain)" ] || problem "the working tree has uncommitted changes"
git -C "$REPO" fetch --quiet "$REMOTE" main --tags || problem "could not fetch $REMOTE"
remote_tags=$(git -C "$REPO" ls-remote --tags "$REMOTE" 2>/dev/null || true)
root_remote=$(echo "$remote_tags" | awk -v t="refs/tags/$VERSION^{}" '$2 == t {print $1}')
[ -n "$root_remote" ] || root_remote=$(echo "$remote_tags" | awk -v t="refs/tags/$VERSION" '$2 == t {print $1}')
case "$(main_check "$SHA" "$(git -C "$REPO" rev-parse "$REMOTE/main" 2>/dev/null || true)" "$root_remote")" in
  main) note "$SHA is $REMOTE/main" ;;
  resuming) note "$SHA is not $REMOTE/main any more, but $VERSION is on $REMOTE at it; resuming" ;;
  *) problem "$SHA is not $REMOTE/main" ;;
esac
CI_BASIS=none
if command -v gh >/dev/null 2>&1; then
  ci=$(ci_status "$GITHUB_REPO" "$SHA" "$(git -C "$REPO" rev-parse "$SHA^{tree}" 2>/dev/null || true)")
  case "$ci" in
    success) note "CI on main is green for $SHA" ;;
    queue) note "no completed CI run on main for $SHA yet, but the merge queue's run of the same tree passed: the preflight and a dry run go on; a --push release waits for main's push run to pass before it tags the root module" ;;
    *) problem "CI for $SHA on main is $ci, not green (and no merge queue run of the same tree passed)" ;;
  esac
  CI_BASIS=$ci
else
  problem "gh is not installed, so CI status for $SHA cannot be checked"
fi
git -C "$REPO" show "$SHA:docs/releases/$VERSION.md" >/dev/null 2>&1 || problem "docs/releases/$VERSION.md does not exist at $SHA"
changelog=$(git -C "$REPO" show "$SHA:CHANGELOG.md" 2>/dev/null || true)
changelog_has "$changelog" "${VERSION#v}" || problem "CHANGELOG.md has no [${VERSION#v}] section at $SHA"
left=$(git -C "$REPO" ls-tree -r --name-only "$SHA" -- changes/unreleased 2>/dev/null | awk '/\.md$/' || true)
[ -z "$left" ] || problem "changelog fragments are left at $SHA (run scripts/release.sh --assemble-changelog $VERSION DATE in the release PR): $(echo $left)"
ROOT_TAGGED=false
if [ -n "$root_remote" ]; then
  [ "$root_remote" = "$SHA" ] || die "$VERSION is already tagged on $REMOTE at $root_remote, not $SHA"
  if $PUSH; then ROOT_TAGGED=true; note "$VERSION is already on $REMOTE at $SHA; resuming"; else problem "$VERSION is already on $REMOTE"; fi
fi
ALREADY=""   # nested modules whose tag is already on the remote (a resumed --push run)
for m in $PUBLISHED; do
  if remote_has_tag "$remote_tags" "refs/tags/$m/$VERSION"; then
    if $PUSH && $ROOT_TAGGED; then ALREADY="$ALREADY $m"; note "$m/$VERSION is already on $REMOTE; resuming"
    else problem "$m/$VERSION is already on $REMOTE"; fi
  fi
done

# ---- scratch clone and resolver --------------------------------------------------------------
CLONE=$WORK/repo
git clone --quiet --no-local --no-checkout "$REPO" "$CLONE"
git -C "$CLONE" checkout --quiet --detach "$SHA"
mkdir -p "$WORK/modcache" "$WORK/empty"
if $PUSH; then
  TARGET=$(git -C "$REPO" remote get-url "$REMOTE")
  # Only the public proxy and checksum database: the release is resolved the way a user resolves it.
  gorun() { env GOWORK=off GOFLAGS=-mod=mod GOMODCACHE="$WORK/modcache" GOPROXY="$PROXY" GOPRIVATE= GONOPROXY= GONOSUMDB= GONOSUMCHECK= GOINSECURE= go "$@"; }
else
  TARGET=$WORK/origin.git
  git init --quiet --bare "$TARGET"
  git -C "$CLONE" push --quiet "$TARGET" "$SHA:refs/heads/main"
  # The bide modules come straight from the scratch origin through git; everything else from the
  # usual proxy. The scratch module cache keeps these unpublished versions out of the real one.
  gorun() {
    env GOWORK=off GOFLAGS="-mod=mod -modcacherw" GOMODCACHE="$WORK/modcache" \
      GOPRIVATE="$ROOT_MODULE" GONOSUMDB="$ROOT_MODULE" GONOPROXY="$ROOT_MODULE" \
      GIT_CONFIG_COUNT=2 \
      GIT_CONFIG_KEY_0="url.file://$TARGET.insteadOf" GIT_CONFIG_VALUE_0="https://$ROOT_MODULE" \
      GIT_CONFIG_KEY_1=protocol.file.allow GIT_CONFIG_VALUE_1=always \
      go "$@"
  }
fi

# wait_resolve module version: the module proxy (or, in a dry run, the scratch origin) serves it.
wait_resolve() {
  local mod=$1 ver=$2 tries=1 i
  $PUSH && tries=60
  for i in $(seq 1 "$tries"); do
    if (cd "$WORK/empty" && gorun list -m "$mod@$ver" >/dev/null 2>&1); then
      note "resolves: $mod@$ver"; return 0
    fi
    [ "$i" -lt "$tries" ] && sleep 10
  done
  (cd "$WORK/empty" && gorun list -m "$mod@$ver") || true
  die "$mod@$ver does not resolve"
}

# ---- 1. the root tag -------------------------------------------------------------------------
say "root: $ROOT_MODULE $VERSION"
if ! $ROOT_TAGGED; then
  # The one step that cannot be undone: on a queue-only basis, main's push run must pass first.
  if $PUSH; then root_tag_gate "$GITHUB_REPO" "$SHA" "$CI_BASIS" || die "not tagging $VERSION"; fi
  git -C "$CLONE" tag -a "$VERSION" -m "bide $VERSION" "$SHA"
  push_tags "$VERSION"
fi
wait_resolve "$ROOT_MODULE" "$VERSION"

# ---- 2. the nested modules, in dependency stages ---------------------------------------------
released=""   # nested dirs released so far
remaining=$PUBLISHED
stage=0
while [ -n "$remaining" ]; do
  stage=$((stage + 1))
  ready="" later=""
  for m in $remaining; do
    ok=true
    for dep in $(bide_requires "$CLONE/$m"); do
      [ "$dep" = "$ROOT_MODULE" ] && continue
      in_list "${dep#"$ROOT_MODULE"/}" $released || ok=false
    done
    if $ok; then ready="$ready $m"; else later="$later $m"; fi
  done
  [ -n "$ready" ] || die "no module in [$remaining ] has all its bide dependencies released; is there a cycle?"
  say "stage $stage:$ready"
  todo=""
  for m in $ready; do
    if in_list "$m" $ALREADY; then wait_resolve "$(module_path "$m")" "$VERSION"; else todo="$todo $m"; fi
  done
  released="$released $ready"
  remaining=$later
  ready=$todo
  [ -n "$ready" ] || continue
  for m in $ready; do
    edits=""
    for dep in $(bide_requires "$CLONE/$m"); do
      edits="$edits -require=$dep@$VERSION -dropreplace=$dep"
    done
    # shellcheck disable=SC2086
    # A module with no bide requirements (a deprecated signpost) has nothing to pin.
    (cd "$CLONE/$m" && { [ -z "$edits" ] || go mod edit $edits; } && gorun mod tidy 2>"$WORK/tidy.log") || { cat "$WORK/tidy.log"; die "$m: go mod tidy failed"; }
    check_gomod "$CLONE/$m"
    (cd "$CLONE/$m" && gorun build ./... && gorun vet ./...)
    if $TEST; then (cd "$CLONE/$m" && gorun test -count=1 ./... >"$WORK/test.log" 2>&1) || { cat "$WORK/test.log"; die "$m: tests failed"; }; fi
    note "$m: requires $(bide_requires "$CLONE/$m" | tr '\n' ' ')at $VERSION; builds$($TEST && echo ", vets and tests" || echo " and vets") with GOWORK=off"
  done
  # shellcheck disable=SC2086
  (cd "$CLONE" && git add -- $(for m in $ready; do echo "$m/go.mod"; [ -f "$m/go.sum" ] && echo "$m/go.sum"; done) \
    && git commit --quiet -s -m "release: $(echo $ready | sed 's/ /, /g') require bide $VERSION" \
      -m "Tagged as$(for m in $ready; do printf ' %s/%s' "$m" "$VERSION"; done). This commit is reachable from those tags only; main keeps its replace directives.")
  commit=$(git -C "$CLONE" rev-parse HEAD)
  note "commit $commit"
  tags=""
  for m in $ready; do
    (cd "$CLONE" && check_tag "$m/$VERSION" >/dev/null)
    git -C "$CLONE" tag -a "$m/$VERSION" -m "bide $m $VERSION" "$commit"
    tags="$tags $m/$VERSION"
  done
  # shellcheck disable=SC2086
  push_tags $tags
  for m in $ready; do wait_resolve "$(module_path "$m")" "$VERSION"; done
done

# ---- 3. verify as a consumer -----------------------------------------------------------------
say "verify: go list -m and a consumer build"
for m in $PUBLISHED; do
  (cd "$WORK/empty" && gorun list -m "$(module_path "$m")@$VERSION") | sed 's/^/    go list -m: /'
done
C=$WORK/consumer
mkdir -p "$C"
(
  cd "$C"
  printf 'module example.com/bide-release-check\n\ngo 1.27.0\n' > go.mod
  consumer_main > main.go
  # shellcheck disable=SC2046
  gorun get "$ROOT_MODULE@$VERSION" $(for m in $(libraries); do echo "$(module_path "$m")@$VERSION"; done) 2>"$WORK/get.log" || { cat "$WORK/get.log"; die "go get failed"; }
  # A command is a tool of the consumer (go get -tool), so tidy keeps it, and it is built as one.
  for m in $COMMANDS; do gorun get -tool "$(module_path "$m")@$VERSION" 2>>"$WORK/get.log" || { cat "$WORK/get.log"; die "go get -tool $m failed"; }; done
  gorun mod tidy 2>/dev/null
  gorun build ./...
  for m in $COMMANDS; do gorun build -o /dev/null "$(module_path "$m")"; done
  got=$(gorun list -m -f '{{.Path}} {{.Version}}' all | grep "^$ROOT_MODULE[ /]")
  echo "$got" | sed 's/^/    consumer selects: /'
  wrong=$(echo "$got" | awk -v v="$VERSION" '$2 != v')
  [ -z "$wrong" ] || die "the consumer selected other versions: $wrong"
)
note "a consumer module requiring every published module at $VERSION builds"

# The released bide command scaffolds a project (bide init) that builds, vets and passes its own test
# against the released modules: the template a user gets resolves against what was published.
say "verify: bide init with the released command"
mkdir -p "$WORK/bin"
GOBIN="$WORK/bin" gorun install "$(module_path cmd/bide)@$VERSION" 2>"$WORK/install.log" || { cat "$WORK/install.log"; die "go install cmd/bide@$VERSION failed"; }
"$WORK/bin/bide" init -module example.com/bide-init-check "$WORK/scaffold" >/dev/null || die "bide init failed"
(cd "$WORK/scaffold" && gorun mod tidy && gorun vet ./... && gorun test -count=1 ./... >"$WORK/scaffold.log" 2>&1) || { cat "$WORK/scaffold.log"; die "the bide init project does not build and pass its test against $VERSION"; }
(cd "$WORK/scaffold" && "$WORK/bin/bide" check -strict flow.yaml) || die "bide check fails on the bide init project"
note "bide init writes a project that requires $VERSION, builds, and passes its test"

# bide-hsm is installed from source as a user installs it (it builds with or without cgo, so this
# needs no C toolchain); its version command runs and names the release it was built from.
say "verify: bide-hsm with the released module"
GOBIN="$WORK/bin" gorun install "$(module_path cmd/bide/bide-hsm)@$VERSION" 2>"$WORK/install.log" || { cat "$WORK/install.log"; die "go install cmd/bide/bide-hsm@$VERSION failed"; }
hsm_version=$("$WORK/bin/bide-hsm" version) || die "bide-hsm version failed"
echo "$hsm_version" | head -1 | grep -qx "bide-hsm $VERSION" || die "bide-hsm version says [$(echo "$hsm_version" | head -1)], want [bide-hsm $VERSION]"
note "bide-hsm $VERSION installs and runs ($(echo "$hsm_version" | sed -n 2p))"

if $PUSH; then
  git -C "$REPO" fetch --quiet "$REMOTE" --tags
  say "released $VERSION: the root and $(echo $PUBLISHED | wc -w | tr -d ' ') nested modules resolve on $PROXY"
else
  say "dry run complete: $VERSION and $(echo $PUBLISHED | wc -w | tr -d ' ') nested tags were made in a scratch clone and pushed only to a scratch origin"
  git -C "$CLONE" log --oneline --decorate "$SHA^..HEAD" | sed 's/^/    /'
  note "run again with --push to release"
fi
