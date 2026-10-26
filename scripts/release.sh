#!/usr/bin/env bash
# release.sh: tag a bide release, the root module and every published nested module.
#
#   scripts/release.sh vX.Y.Z [--push] [--ref REF] [--remote NAME] [--no-test] [--keep]
#   scripts/release.sh --check-tag TAG     (CI: the module a nested tag names is releasable)
#   scripts/release.sh --check-classification
#   scripts/release.sh --list-published | --list-commands | --consumer-main   (scripts/module-sbom.sh)
#   scripts/release.sh --assemble-changelog vX.Y.Z[-rc.N] YYYY-MM-DD   (release PR: fragments into CHANGELOG.md)
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
# A patch release of an older line (v0.23.1 after v0.24.0 is on main) is released from the branch
# release/v0.23 instead of main: see release_branch_gate and CONTRIBUTING.md (Releasing, Patch releases).
# REF must then be the tip of $REMOTE/release/vX.Y, VERSION the next patch above the line's newest tag,
# the branch PR-only with required status checks (approvals are not checked), and that branch's own ci.yml push run of
# REF green. The steps below are the same; the queue fallback does not apply.
#
# The release commits are reachable from their tags only; main keeps its replace directives, so
# development is unchanged. Repo-only modules (examples, integration, benchmarks) are never tagged.
#
# A --push run that stopped (waiting for the proxy, say) is run again with the same arguments; it
# reuses the tags already on the remote. It resumes even when main has moved since the root tag was
# pushed (a pull request merged meanwhile), as long as the root tag points at the commit released.
#
# Nobody should `go get`, `go run` or `go list` a version before its tag is pushed: the module proxy
# caches the miss for 30 to 60 minutes, however often it is asked again, and the release then waits
# until the cached miss expires. So after a push the script checks each tag with GOPROXY=direct (the
# proxy and the checksum database never hear of it), does not ask the proxy until PW_SETTLE seconds
# (300) after the push, then asks with growing waits (5, 10, 15 minutes) for at most PW_MAX seconds
# (5400), saying what it waits for; scripts/proxy-wait.sh has the rules and the variables. A run that
# gave up is run again with the same arguments.
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
# shellcheck source=scripts/proxy-wait.sh
. "$(cd "$(dirname "$0")" && pwd)/proxy-wait.sh"

# Libraries users import. Each is tagged <dir>/vX.Y.Z at every release.
PUBLISHED="govern store/sqlite store/postgres mcptools trace codec/gcf anchor/awss3 anchor/gcs govern/sqlitelog govern/redislog govern/postgreslog signer/pkcs11 signer/awskms signer/azurekv signer/gcpkms flowedit cmd/bide cmd/bide/bide-hsm cmd/bide-anchor"
# Published modules whose root package is a command (package main), installed with go install. The
# consumer check builds them instead of importing them, since a program cannot be imported.
# cmd/bide-view, a signpost to bide view, was tagged at every release through v0.25.0 and is removed.
# cmd/bide/bide-hsm is bide approve for keys in an HSM or a KMS; it needs cgo, so it is released as
# source (go install) and not as a prebuilt binary (see .goreleaser.yaml). It lives under cmd/bide
# so that it can use that module's internal approvecmd package.
COMMANDS="cmd/bide cmd/bide/bide-hsm cmd/bide-anchor"
# Modules that exist only to keep their dependencies out of the core. They keep their replace
# directives and are never tagged.
REPO_ONLY="cmd/bide/bide-hsm/azuretest cmd/bide/bide-hsm/emulatortest signer/gcpkms/emulatortest signer/awskms/emulatortest examples/approval examples/govern examples/hsm-signing examples/mcp examples/observability examples/plan examples/sso-certs examples/stripe-refund integration benchmarks"

# gorun runs the go command the way the release resolves modules; the release flow below replaces it.
gorun() { env GOWORK=off go "$@"; }

die() { echo "release: error: $*" >&2; exit 1; }
say() { echo "==> $*"; }
note() { echo "    $*"; }

usage() { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

# is_version v: a semantic version with a leading v and an optional pre-release, the one pattern
# module-sbom.sh (VERSION_RX) and the changelog tool use: no leading zeros in the numbers or in a
# numeric identifier, no empty identifier, no build metadata. A version it accepts is accepted by
# the later steps; one it refuses stops the release here, not late.
is_version() {
  local id='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-$id(\.$id)*)?$ ]]
}

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
# candidate_before text version: prints the newest release heading of the changelog text when version
# is a final release (no pre-release) and that heading is a candidate of the same version: the tool
# assembles only the fragments added since it, so the final's section would be short.
candidate_before() {
  local head
  head=$(awk '/^## \[[0-9]/ {print; exit}' <<<"$1")
  [[ "$2" != *-* && "$head" == "## [${2#v}-"* ]] && echo "$head"
  return 0
}

assemble_changelog() {
  is_version "$1" || die "$1 is not a version like v0.8.0"
  local cand
  cand=$(candidate_before "$(cat "$(git rev-parse --show-toplevel)/CHANGELOG.md")" "$1")
  [ -z "$cand" ] || echo "release: WARNING: the newest release in CHANGELOG.md is a candidate ($cand): the assembled $1 section lists only the fragments since it. Merge the candidate's section into it and point its compare link at the previous final release (CONTRIBUTING.md, Releasing)." >&2
  (cd "$(git rev-parse --show-toplevel)" && go run ./internal/tools/changelog assemble -version "${1#v}" -date "$2")
}

# scratch_clone repo clone sha: a clone of repo at clone, detached at sha. A clone copies only the
# local branches and the tags, so a sha reachable only through a remote-tracking ref (a release of
# origin/release/v0.23 from a checkout with no such local branch) is fetched by name.
scratch_clone() {
  git clone --quiet --no-local --no-checkout "$1" "$2"
  git -C "$2" cat-file -e "$3^{commit}" 2>/dev/null || git -C "$2" fetch --quiet --no-tags "$1" "$3"
  git -C "$2" checkout --quiet --detach "$3"
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

# release_branch_check version sha tip latest: whether a release of sha, with tip the commit at the
# remote's release/vX.Y (empty when the branch is missing) and latest the Z of the newest plain
# vX.Y.Z root tag on that line (empty when none), is a release-branch release: "ok", or the reason
# it is not. The version must be a plain patch (Z above 0) that is the next one after the newest
# tag of its line, and sha must be the branch tip exactly: an ancestor of the tip, another branch,
# a tag or an unmerged commit is not.
release_branch_check() {
  local v=$1 sha=$2 tip=$3 latest=$4 z line
  [[ "$v" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo "$v is not a plain release version"; return; }
  line=${BASH_REMATCH[1]}.${BASH_REMATCH[2]} z=${BASH_REMATCH[3]}
  [ "$z" -gt 0 ] || { echo "$v is not a patch release (a new line is released from main)"; return; }
  [ -n "$tip" ] || { echo "the remote has no branch release/v$line"; return; }
  [ "$tip" = "$sha" ] || { echo "$sha is not the tip of release/v$line ($tip)"; return; }
  [ -n "$latest" ] || { echo "the line v$line has no release tag"; return; }
  [[ "$latest" =~ ^(0|[1-9][0-9]*)$ ]] || { echo "the newest tag of v$line is not a plain number"; return; }
  [ "$z" -eq $((latest + 1)) ] || { echo "$v is not the next patch of v$line.$latest"; return; }
  echo ok
}

# line_latest tags X.Y: the Z of the newest plain vX.Y.Z root tag in the `git ls-remote --tags`
# output (a pre-release, a nested module's tag and an annotated tag's peeled line do not count),
# empty when there is none.
line_latest() {
  awk -v p="refs/tags/v$2." '
    index($2, p) == 1 { z = substr($2, length(p) + 1); sub(/\^\{\}$/, "", z) }
    index($2, p) == 1 && z ~ /^(0|[1-9][0-9]*)$/ && (best == "" || z + 0 > best + 0) { best = z }
    END { print best }' <<<"$1"
}

# branch_pr_only REPO BRANCH: the branch is closed to direct pushes: it requires pull requests and
# status checks through a ruleset with no bypass actor, or through classic protection that applies to
# administrators too. This is "changes arrive by pull request with the required checks", not a count
# of approvals (bide's rulesets ask for none). It cannot see how commits landed before the rule
# existed; branch_commits_via_pr checks that. Anything else, or GitHub not answering, is no.
branch_pr_only() {
  local out id
  out=$(gh api "repos/$1/rules/branches/$2" --jq '[.[] | .type] | if (index("pull_request") and index("required_status_checks")) then (map(select(. == "pull_request")) | length) else 0 end' 2>/dev/null) || out=0
  if [ "${out:-0}" -gt 0 ]; then
    # Every ruleset that applies must have no bypass actor: an administrator could push around it.
    for id in $(gh api "repos/$1/rules/branches/$2" --jq '[.[].ruleset_id] | unique | .[]' 2>/dev/null || echo bad); do
      [[ "$id" =~ ^[0-9]+$ ]] || return 1
      out=$(gh api "repos/$1/rulesets/$id" --jq '.bypass_actors | length' 2>/dev/null) || return 1
      [ "$out" = 0 ] || return 1
    done
    return 0
  fi
  out=$(gh api "repos/$1/branches/$2/protection" --jq 'if .required_pull_request_reviews and .required_status_checks and .enforce_admins.enabled then "yes" else "no" end' 2>/dev/null) || out=no
  [ "$out" = yes ]
}

# branch_commits_via_pr REPO BRANCH FROM TO: every commit on the first-parent chain FROM..TO (in
# $REPO) is the merge of a pull request merged into BRANCH, per GitHub, and there is at least one.
# A branch that took direct pushes before it was protected fails here.
branch_commits_via_pr() {
  local c n=0
  for c in $(git -C "$REPO" rev-list --first-parent "$3..$4"); do
    n=$((n + 1))
    [ "$(gh api "repos/$1/commits/$c/pulls" --jq '[.[] | select(.merged_at != null and .base.ref == "'"$2"'")] | length' 2>/dev/null || echo 0)" -gt 0 ] || { echo "commit $c on $2 is not the merge of a pull request into it" >&2; return 1; }
  done
  [ "$n" -gt 0 ]
}

# release_branch_gate version sha: the release-branch mode of the preflight. Prints why on stderr
# and returns 1 unless release_branch_check passes, sha descends from the line's newest tag, the
# branch is closed to direct pushes (branch_pr_only), every commit since the tag is the merge of a
# pull request into it (branch_commits_via_pr), and (checked by the caller with the branch's own
# push run) CI is green. Reads $REMOTE, $REPO, $remote_tags, $GITHUB_REPO; sets REL_BRANCH on success.
release_branch_gate() {
  local v=$1 sha=$2 line branch tip latest why tagsha
  [[ "$v" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.[0-9]+$ ]] || return 1
  line=${BASH_REMATCH[1]}.${BASH_REMATCH[2]} branch=release/v$line
  tip=$(git -C "$REPO" ls-remote "$REMOTE" "refs/heads/$branch" 2>/dev/null | awk -v r="refs/heads/$branch" '$2 == r {print $1}')
  latest=$(line_latest "$remote_tags" "$line")
  why=$(release_branch_check "$v" "$sha" "$tip" "$latest")
  [ "$why" = ok ] || { echo "$why" >&2; return 1; }
  # The line's newest tag is the remote's (the peeled commit of an annotated tag), never a local tag of the same name.
  tagsha=$(awk -v t="refs/tags/v$line.$latest" '$2 == t "^{}" {print $1; exit} $2 == t {plain = $1} END {if (plain != "") print plain}' <<<"$remote_tags" | head -1)
  [ -n "$tagsha" ] || { echo "v$line.$latest is not on $REMOTE" >&2; return 1; }
  git -C "$REPO" fetch --quiet "$REMOTE" "refs/heads/$branch" "refs/tags/v$line.$latest" 2>/dev/null || { echo "could not fetch $REMOTE $branch" >&2; return 1; }
  git -C "$REPO" merge-base --is-ancestor "$tagsha" "$sha" 2>/dev/null || { echo "$sha does not descend from v$line.$latest ($tagsha)" >&2; return 1; }
  command -v gh >/dev/null 2>&1 || { echo "gh is not installed, so $branch cannot be checked" >&2; return 1; }
  branch_pr_only "$GITHUB_REPO" "$branch" || { echo "$branch is not closed to direct pushes (it needs pull requests and status checks, no bypass actor, enforced for administrators)" >&2; return 1; }
  branch_commits_via_pr "$GITHUB_REPO" "$branch" "$tagsha" "$sha" || return 1
  REL_BRANCH=$branch
}

# ci_status REPO SHA TREE [BRANCH]: whether CI passed for the release commit SHA, whose tree is TREE:
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
# With BRANCH (a release branch, which has no merge queue) only that branch's push runs count and
# the queue is not consulted: "success", "failure", "none" or "unknown".
# Trees are compared, not commit SHAs: the queue's commit is not the commit that lands on main.
# A queue run that failed does not count, however many; a different tree does not count either.
ci_status() {
  local repo=$1 sha=$2 tree=$3 branch=${4:-} push heads head t
  push=$(push_ci_state "$repo" "$sha" "$branch")
  case "$push" in success|failure) echo "$push"; return ;; unknown) echo unknown; return ;; esac
  [ -z "$branch" ] || { echo none; return; }
  heads=$(gh run list -R "$repo" --workflow ci.yml --event merge_group --status success --limit 100 --json headSha --jq '.[].headSha' 2>/dev/null) || { echo unknown; return; }
  for head in $heads; do
    t=$(gh api "repos/$repo/git/commits/$head" --jq .tree.sha 2>/dev/null) || continue
    if [ "$t" = "$tree" ]; then echo "queue"; return; fi
  done
  echo none
}

# ci_wait REPO SHA TREE [BRANCH]: ci_status, but for a --push release a "none" (no completed push run of SHA
# yet, and no queue run of the tree) is waited out, because right after an admin merge main's push
# CI has often not started or is still going. It polls every BIDE_CI_PREFLIGHT_POLL_SECS (30) for up
# to BIDE_CI_PREFLIGHT_WAIT_SECS (2700) and prints the answer ci_status gives when the time is up.
# Any other answer (success, queue, failure, unknown) is returned at once. A dry run (PUSH=false)
# asks once. Progress goes to stderr.
ci_wait() {
  local repo=$1 sha=$2 tree=$3 branch=${4:-} poll=${BIDE_CI_PREFLIGHT_POLL_SECS:-30} wait=${BIDE_CI_PREFLIGHT_WAIT_SECS:-2700} n i st
  n=$(awk -v w="$wait" -v p="$poll" 'BEGIN { n = int(w / p); if (n < 1) n = 1; print n }')
  $PUSH || n=1
  for ((i = 1; i <= n; i++)); do
    st=$(ci_status "$repo" "$sha" "$tree" "$branch")
    [ "$st" = none ] || { echo "$st"; return; }
    if [ "$i" -lt "$n" ]; then
      note "${branch:-main}'s CI for $sha has not completed (poll $i of $n, every ${poll}s, up to $((wait / 60)) min); waiting" >&2
      sleep "$poll"
    fi
  done
  $PUSH && note "${branch:-main}'s CI for $sha still has not completed after $((wait / 60)) min" >&2
  echo none
}

# ci_code_sha SHA: the commit whose push run decides whether SHA may be released. CI skips the Go
# jobs for a push of one documentation-only commit (ci.yml, Changes), so a documentation-only SHA
# on top of a red code commit has a green push run of its own. This walks back over first parents
# while the commit changes only documentation (changed-code.sh, the classification CI uses) and
# prints the first commit that does not: the last one that ran the Go jobs, whose own push run
# must have passed. A commit that cannot be read counts as code (it is printed); a walk of more
# than 200 commits, or a root, fails (the caller treats that as "unknown"). It reads the
# repository in $REPO and notes the walk on stderr.
ci_code_sha() {
  local sha=$1 start=$1 n=0 files classify git=(git -C "${REPO:-.}")
  classify="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../.github/scripts/changed-code.sh"
  while :; do
    # changed-code.sh exits 0 when the files include code (and for no files at all), 1 for
    # documentation only. A commit that cannot be diffed (unreadable, a root) counts as code.
    if files=$("${git[@]}" diff --name-only "$sha^" "$sha" 2>/dev/null) && ! echo "$files" | "$classify" >/dev/null; then
      sha=$("${git[@]}" rev-parse "$sha^") || return 1
      n=$((n + 1))
      [ "$n" -le 200 ] || { echo "release: 200 documentation-only commits below $start; cannot find the commit whose CI ran the Go jobs" >&2; return 1; }
      continue
    fi
    [ "$sha" = "$start" ] || echo "release: $start is documentation only down to $sha's child, whose CI runs skipped the Go jobs; checking the push run of $sha" >&2
    echo "$sha"; return 0
  done
}

# push_ci_state REPO SHA [BRANCH]: the state of main's ci.yml push runs of SHA (only those on BRANCH
# when it is given, a release branch; any branch otherwise) (of the nearest commit at or
# below SHA that changes code, ci_code_sha, when SHA is documentation only): "failure" when a completed one
# failed or was cancelled, "running" when one has not completed, "success" when all completed and
# passed, "none" when there is none yet, "unknown" when GitHub could not be queried.
push_ci_state() {
  local sha branch_arg=()
  [ -z "${3:-}" ] || branch_arg=(--branch "$3")
  sha=$(ci_code_sha "$2") || { echo unknown; return; }
  gh run list -R "$1" --workflow ci.yml --commit "$sha" --event push ${branch_arg[@]+"${branch_arg[@]}"} --json status,conclusion \
    --jq 'if length == 0 then "none" elif any(.[]; .status == "completed" and .conclusion != "success") then "failure" elif any(.[]; .status != "completed") then "running" else "success" end' 2>/dev/null || echo unknown
}

# root_tag_gate REPO SHA BASIS [BRANCH]: run just before the root tag is pushed, which cannot be undone (the
# module proxy keeps it). BASIS is what the preflight found ("success" or "queue", see ci_status).
# It does not trust the preflight alone: it polls (the first poll answers at once when the push run
# has passed), so a push run started since, a manual re-run, is waited for, and one that then fails
# stops the release even though an earlier run passed. It polls every
# BIDE_CI_POLL_SECS (30) for up to BIDE_CI_WAIT_SECS (3600), for main's push run of SHA to complete,
# and passes only if it succeeded; a failed run, or one still going or missing when the time is up,
# stops the release before anything is tagged. With BRANCH only that branch's push runs count.
# Returns 0 to go on, 1 to stop.
root_tag_gate() {
  local repo=$1 sha=$2 basis=$3 branch=${4:-} poll=${BIDE_CI_POLL_SECS:-30} wait=${BIDE_CI_WAIT_SECS:-3600} n i st
  n=$(awk -v w="$wait" -v p="$poll" 'BEGIN { n = int(w / p); if (n < 1) n = 1; print n }')
  [ "$basis" != queue ] || note "the preflight rested on the merge queue's run of the same tree, which is weaker than main's push run" >&2
  note "checking ${branch:-main}'s CI push run of $sha before tagging (waits up to $((wait / 60)) min for one that is still going)" >&2
  for ((i = 1; i <= n; i++)); do
    st=$(push_ci_state "$repo" "$sha" "$branch")
    case "$st" in
      success) note "${branch:-main}'s CI push run of $sha passed" >&2; return 0 ;;
      failure) echo "release: ${branch:-main}'s CI push run of $sha failed; the merge queue's run passed, but the release stops before tagging" >&2; return 1 ;;
    esac
    [ "$i" -lt "$n" ] && sleep "$poll"
  done
  echo "release: ${branch:-main}'s CI push run of $sha is $st after $((wait / 60)) min; nothing was tagged (re-run the release when it has passed)" >&2
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
      PUSHED_AT=$(date +%s)
      if $PUSH; then note "pushed ${batch[*]}"; else note "pushed to the scratch origin: ${batch[*]}"; fi
      batch=()
    fi
  done
}

# finish_work dir keep status: runs when the script ends. The work tree (a scratch clone and a module
# cache of about 2.5 GB, its files read-only) is removed when the run succeeded; it is kept, with its
# path, when the run failed or --keep was given. A --push run that stopped is run again from scratch,
# so nothing in the tree is needed to resume.
finish_work() {
  if [ "$2" = true ] || [ "$3" -ne 0 ]; then
    echo "release: work tree kept at $1"
  else
    chmod -R u+w "$1" 2>/dev/null
    rm -rf "$1"
  fi
}

# work_traps: finish_work runs when the script ends, with the script's exit status. An interrupt or a
# termination is a failed run: bash 3.2 hands the EXIT trap a status of 0 for them, which would delete
# the tree, so they exit non-zero first.
work_traps() {
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'finish_work "$WORK" "$KEEP" $?' EXIT
}

# self_test_work: the work tree is removed after a successful run, read-only files included, and kept
# with its path after a failed one or with --keep.
self_test_work() {
  local d out ok=true
  d=$(mktemp -d); mkdir -p "$d/modcache/x"; : >"$d/modcache/x/f"; chmod a-w "$d/modcache/x/f" "$d/modcache/x"
  out=$(finish_work "$d" false 0)
  { [ ! -e "$d" ] && [ -z "$out" ]; } || { echo "self-test: a successful run left its work tree"; ok=false; chmod -R u+w "$d"; rm -rf "$d"; }
  d=$(mktemp -d)
  out=$(finish_work "$d" false 1)
  { [ -d "$d" ] && [ "$out" = "release: work tree kept at $d" ]; } || { echo "self-test: a failed run did not keep its work tree and say where: [$out]"; ok=false; }
  rm -rf "$d"
  d=$(mktemp -d)
  out=$(finish_work "$d" true 0)
  { [ -d "$d" ] && [ "$out" = "release: work tree kept at $d" ]; } || { echo "self-test: --keep did not keep the work tree: [$out]"; ok=false; }
  rm -rf "$d"
  # An interrupt or a termination keeps the tree, as any failure does: a child shell with the same traps
  # signals itself, and bash 3.2 would hand the EXIT trap a status of 0 without the INT and TERM traps.
  local sig
  for sig in INT TERM; do
    d=$(mktemp -d)
    out=$("$BASH" -c "$(declare -f finish_work work_traps); WORK=$d KEEP=false; work_traps; kill -$sig \$\$; sleep 5" 2>/dev/null)
    { [ -d "$d" ] && [ "$out" = "release: work tree kept at $d" ]; } || { echo "self-test: a run ended by $sig did not keep its work tree: [$out]"; ok=false; }
    rm -rf "$d"
  done
  $ok
}

# self_test_release_branch TMP: the release-branch mode (release_branch_check, line_latest,
# branch_pr_only, release_branch_gate, and the branch filter of the CI status) against the fake gh
# that self_test_ci installed in TMP/bin and a scratch repository that stands in for the remote.
self_test_release_branch() {
  local tmp=$1 ok=true got fx=$1/fx
  rb_case() { # rb_case ok|no DESCRIPTION VERSION SHA TIP LATEST
    got=$(release_branch_check "$3" "$4" "$5" "$6")
    if [ "$1" = ok ]; then [ "$got" = ok ] || { echo "self-test: release branch: $2: refused ($got)"; ok=false; }
    else [ "$got" != ok ] || { echo "self-test: release branch: $2: accepted"; ok=false; }; fi
  }
  local a b
  a=$(printf '%040d' 1); b=$(printf '%040d' 2)
  rb_case ok "the next patch at the branch tip" v0.23.1 "$a" "$a" 0
  rb_case ok "the next patch after a patch" v0.23.3 "$a" "$a" 2
  rb_case no "a commit that is not the tip" v0.23.1 "$a" "$b" 0
  rb_case no "no branch" v0.23.1 "$a" "" 0
  rb_case no "no tag on the line" v0.23.1 "$a" "$a" ""
  rb_case no "a skipped patch" v0.23.2 "$a" "$a" 0
  rb_case no "a patch already released" v0.23.1 "$a" "$a" 1
  rb_case no "a new line (Z is 0)" v0.24.0 "$a" "$a" 0
  rb_case no "a release candidate" v0.23.1-rc.1 "$a" "$a" 0
  rb_case no "a leading zero in the newest patch" v0.23.1 "$a" "$a" 00
  local tags
  tags=$(printf '%s\trefs/tags/%s\n' "$a" v0.22.0 "$a" v0.23.0 "$b" 'v0.23.0^{}' "$a" v0.23.1-rc.1 "$a" v0.23.10 "$a" v0.23.2 "$a" govern/v0.23.9 "$a" v0.230.99 "$a" v0.23.x)
  [ "$(line_latest "$tags" 0.23)" = 10 ] || { echo "self-test: release branch: line_latest said [$(line_latest "$tags" 0.23)], want 10 (numeric order; no candidate, nested tag or other line)"; ok=false; }
  [ -z "$(line_latest "$tags" 0.24)" ] || { echo "self-test: release branch: line_latest found a tag on a line without one"; ok=false; }
  [ "$(line_latest "$(printf '%s\trefs/tags/v0.23.0\n' "$a")" 0.23)" = 0 ] || { echo "self-test: release branch: line_latest missed v0.23.0"; ok=false; }

  # branch_pr_only: a ruleset with pull_request and required_status_checks rules and no bypass actor,
  # or classic protection with required reviews, status checks and enforcement for administrators.
  rm -f "$fx/down" "$fx"/rules-*.json "$fx"/protection-*.json "$fx"/ruleset-*.json
  rev_case() { # rev_case yes|no DESCRIPTION
    local r=no; FX=$fx PATH="$tmp/bin:$PATH" branch_pr_only example/r release/v0.23 && r=yes
    [ "$r" = "$1" ] || { echo "self-test: release branch: $2: branch_pr_only said $r, want $1"; ok=false; }
  }
  local rules_ok='[{"type":"deletion","ruleset_id":7},{"type":"pull_request","ruleset_id":7},{"type":"required_status_checks","ruleset_id":7}]'
  rev_case no "no rule and no protection (GitHub answers 404)"
  printf '[{"type":"deletion","ruleset_id":7},{"type":"non_fast_forward","ruleset_id":7}]' > "$fx/rules-release_v0.23.json"
  rev_case no "a ruleset without a pull_request rule"
  printf '[{"type":"pull_request","ruleset_id":7}]' > "$fx/rules-release_v0.23.json"; printf '{"bypass_actors":[]}' > "$fx/ruleset-7.json"
  rev_case no "a ruleset with pull requests but no status checks"
  printf '%s' "$rules_ok" > "$fx/rules-release_v0.23.json"
  rev_case yes "a ruleset with pull requests and status checks, no bypass actor"
  printf '{"bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole"}]}' > "$fx/ruleset-7.json"
  rev_case no "the same ruleset with a bypass actor"
  rm -f "$fx/ruleset-7.json"
  rev_case no "a ruleset whose bypass list cannot be read"
  printf '{"bypass_actors":[]}' > "$fx/ruleset-7.json"
  : > "$fx/down"; rev_case no "GitHub unreachable"; rm -f "$fx/down"
  rm -f "$fx/rules-release_v0.23.json"
  printf '{"required_status_checks":{}}' > "$fx/protection-release_v0.23.json"
  rev_case no "classic protection without required reviews"
  printf '{"required_pull_request_reviews":{},"required_status_checks":{},"enforce_admins":{"enabled":false}}' > "$fx/protection-release_v0.23.json"
  rev_case no "classic protection that administrators may skip"
  printf '{"required_pull_request_reviews":{},"required_status_checks":{},"enforce_admins":{"enabled":true}}' > "$fx/protection-release_v0.23.json"
  rev_case yes "classic protection with reviews, checks and enforcement for administrators"
  rm -f "$fx/protection-release_v0.23.json"

  # The CI status counts only the release branch's own push runs, and never the merge queue.
  local pass='[{"status":"completed","conclusion":"success"}]' failed='[{"status":"completed","conclusion":"failure"}]' T=tree-of-release
  rm -f "$fx"/branch.json "$fx"/tree-*; printf '[]' > "$fx/empty.json"
  printf '%s\n' "$T" > "$fx/tree-q-same"
  br_case() { # br_case WANT DESCRIPTION PUSH-JSON BRANCH-JSON MERGE-GROUP-JSON [BRANCH]
    printf '%s' "$3" > "$fx/push.json"; printf '%s' "$5" > "$fx/mg.json"
    if [ -n "$4" ]; then printf '%s' "$4" > "$fx/branch.json"; else rm -f "$fx/branch.json"; fi
    got=$(FX=$fx PATH="$tmp/bin:$PATH" ci_status example/r release-sha "$T" "${6-release/v0.23}")
    [ "$got" = "$1" ] || { echo "self-test: release branch: $2: ci_status said $got, want $1"; ok=false; }
  }
  local qsame='[{"conclusion":"success","headSha":"q-same"}]'
  br_case success "a green run on the release branch" "$pass" "$pass" '[]'
  br_case none "a green run on another branch of the same commit" "$pass" "" '[]'
  br_case failure "a failed run on the release branch" "$pass" "$failed" '[]'
  br_case none "a green queue run of the same tree is not enough" '[]' "" "$qsame"
  br_case success "without a branch the runs of any branch count (main, unchanged)" "$pass" "" '[]' ""
  br_case queue "without a branch the queue still counts (main, unchanged)" '[]' "" "$qsame" ""
  gate_branch() { # gate_branch go|stop DESCRIPTION: root_tag_gate with the files br_case left
    got=$(FX=$fx PATH="$tmp/bin:$PATH" BIDE_CI_POLL_SECS=0.1 BIDE_CI_WAIT_SECS=1 root_tag_gate example/r release-sha success release/v0.23 >/dev/null 2>&1 && echo go || echo stop)
    [ "$got" = "$1" ] || { echo "self-test: release branch: $2: root_tag_gate said $got, want $1"; ok=false; }
  }
  printf '%s' "$pass" > "$fx/push.json"; rm -f "$fx/branch.json"
  gate_branch stop "a green run on another branch only"
  printf '%s' "$pass" > "$fx/branch.json"
  gate_branch go "a green run on the release branch"
  printf '%s' "$failed" > "$fx/branch.json"
  gate_branch stop "a failed run on the release branch"

  # release_branch_gate against real commits: main has the line's tag and a later commit; the release
  # branch is the tag plus a fix; "other" is an unrelated root commit.
  local rr="$tmp/rb-remote.git" wc="$tmp/rb-work" cl="$tmp/rb-clone" g=(-c user.name=t -c user.email=t@example.com -c commit.gpgsign=false) tag fix main2 other
  git init --quiet --bare "$rr"; git init --quiet "$wc"
  echo 0 > "$wc/f"; git -C "$wc" add f; git -C "$wc" "${g[@]}" commit --quiet -m base
  tag=$(git -C "$wc" rev-parse HEAD); git -C "$wc" tag v0.23.0
  echo 1 > "$wc/f"; git -C "$wc" "${g[@]}" commit --quiet -am "main work"; main2=$(git -C "$wc" rev-parse HEAD)
  git -C "$wc" branch -f main
  git -C "$wc" checkout --quiet -b release/v0.23 "$tag"; echo fix > "$wc/g"; git -C "$wc" add g; git -C "$wc" "${g[@]}" commit --quiet -m fix
  fix=$(git -C "$wc" rev-parse HEAD)
  git -C "$wc" checkout --quiet --orphan unrelated; git -C "$wc" "${g[@]}" commit --quiet --allow-empty -m orphan; other=$(git -C "$wc" rev-parse HEAD)
  git -C "$wc" push --quiet "$rr" main release/v0.23 unrelated v0.23.0
  git clone --quiet "$rr" "$cl" 2>/dev/null
  rm -f "$fx"/rules-*.json "$fx"/protection-*.json
  printf '%s' "$rules_ok" > "$fx/rules-release_v0.23.json"
  local merged_pr='[{"merged_at":"2026-10-24T00:00:00Z","base":{"ref":"release/v0.23"}}]'
  printf '%s' "$merged_pr" > "$fx/pulls-$fix.json"
  gate_case() { # gate_case WANT DESCRIPTION VERSION SHA
    got=$( (REPO=$cl REMOTE=origin GITHUB_REPO=example/r REL_BRANCH=; export FX=$fx PATH="$tmp/bin:$PATH"
            remote_tags=$(git -C "$cl" ls-remote --tags origin)
            if release_branch_gate "$3" "$4" 2>"$tmp/gate.err"; then echo "yes:$REL_BRANCH"; else echo no; fi) )
    [ "$got" = "$1" ] || { echo "self-test: release branch: $2: the gate said $got, want $1 ($(tail -1 "$tmp/gate.err" 2>/dev/null))"; ok=false; }
  }
  gate_case yes:release/v0.23 "the tip of the PR-only release branch, the next patch" v0.23.1 "$fix"
  gate_case no "main's own commit (not the branch tip)" v0.23.1 "$main2"
  gate_case no "the commit the branch started from" v0.23.1 "$tag"
  gate_case no "a commit that is not on the release branch" v0.23.1 "$other"
  gate_case no "the tip, but not the next patch" v0.23.2 "$fix"
  gate_case no "the tip, but a new line" v0.24.0 "$fix"
  rm -f "$fx/rules-release_v0.23.json"
  gate_case no "the tip of a branch that is not closed to direct pushes" v0.23.1 "$fix"
  printf '%s' "$rules_ok" > "$fx/rules-release_v0.23.json"
  printf '[]' > "$fx/pulls-$fix.json"
  gate_case no "a tip that is no pull request's merge (a direct push)" v0.23.1 "$fix"
  printf '[{"merged_at":null,"base":{"ref":"release/v0.23"}}]' > "$fx/pulls-$fix.json"
  gate_case no "a tip whose pull request was never merged" v0.23.1 "$fix"
  printf '[{"merged_at":"2026-10-24T00:00:00Z","base":{"ref":"main"}}]' > "$fx/pulls-$fix.json"
  gate_case no "a tip merged into another branch" v0.23.1 "$fix"
  printf '%s' "$merged_pr" > "$fx/pulls-$fix.json"
  git -C "$wc" push --quiet --force "$rr" "$other:refs/heads/release/v0.23"
  printf '%s' "$merged_pr" > "$fx/pulls-$other.json"
  git -C "$wc" push --quiet --force "$rr" "$tag:refs/heads/release/v0.23"
  printf '%s' "$merged_pr" > "$fx/pulls-$tag.json"
  gate_case no "a branch with no commit since the tag (the tag's own commit as the next patch)" v0.23.1 "$tag"
  gate_case no "a branch tip that does not descend from the line's tag" v0.23.1 "$other"
  git -C "$cl" fetch --quiet origin; git -C "$cl" tag -f v0.23.0 "$other" >/dev/null
  gate_case no "a local tag moved onto the tip does not stand in for the remote's tag" v0.23.1 "$other"
  rm -f "$fx"/rules-*.json

  # scratch_clone: a commit reachable only through a remote-tracking ref (a checkout whose only local
  # branch is the default one) is in the clone.
  local sc="$tmp/sc-checkout" sc_sha scc="$tmp/sc-clone"
  git -C "$wc" push --quiet --force "$rr" "$fix:refs/heads/release/v0.23"
  git clone --quiet "$rr" "$sc" 2>/dev/null
  sc_sha=$(git -C "$sc" rev-parse refs/remotes/origin/release/v0.23)
  [ "$sc_sha" = "$fix" ] || { echo "self-test: release branch: the scratch case is not the fix commit"; ok=false; }
  git -C "$sc" rev-list --branches --tags | grep -q "$sc_sha" && { echo "self-test: release branch: the scratch case is not remote-tracking only"; ok=false; }
  scratch_clone "$sc" "$scc" "$sc_sha" >/dev/null 2>&1 || { echo "self-test: release branch: scratch_clone failed for a commit reachable only through a remote-tracking ref"; ok=false; }
  [ "$(git -C "$scc" rev-parse HEAD 2>/dev/null)" = "$sc_sha" ] || { echo "self-test: release branch: the scratch clone is not at the remote-tracking commit"; ok=false; }
  $ok
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
expr= prev= status= commit= branch=
for a in "$@"; do
  [ "$prev" = --branch ] && branch=$a
  [ "$prev" = --jq ] && expr=$a
  [ "$prev" = --commit ] && commit=$a
  [ "$prev" = --status ] && status=$a
  prev=$a
done
case "$*" in
  *"--event push"*)
    f=$FX/push.json
    if [ -n "$branch" ]; then # runs on one branch: $FX/branch.json, none when the file is absent
      f=$FX/branch.json; [ -e "$f" ] || f=$FX/empty.json
    elif [ -n "$commit" ] && [ -e "$FX/run-$commit.json" ]; then # a per-commit run (the documentation-only cases)
      f=$FX/run-$commit.json
    elif [ -e "$FX/push.1.json" ]; then # a sequence: one file per call, the last repeats
      n=$(( $(cat "$FX/n" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FX/n"
      f=$FX/push.$n.json; [ -e "$f" ] || { n=$(ls "$FX"/push.[0-9]*.json | wc -l | tr -d ' '); f=$FX/push.$n.json; }
    fi ;;
  *"--event merge_group"*) f=$FX/mg.json ;;
  "api repos/"*"/rules/branches/"*) f=$FX/rules-$(tr / _ <<<"${2##*/rules/branches/}").json; [ -f "$f" ] || exit 1 ;;
  "api repos/"*"/branches/"*"/protection"*) b=${2#*/branches/}; f=$FX/protection-$(tr / _ <<<"${b%/protection}").json; [ -f "$f" ] || exit 1 ;;
  "api repos/"*"/rulesets/"*) f=$FX/ruleset-${2##*/}.json; [ -f "$f" ] || exit 1 ;;
  "api repos/"*"/commits/"*"/pulls"*) c=${2#*/commits/}; f=$FX/pulls-${c%/pulls}.json; [ -f "$f" ] || exit 1 ;;
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
  # ci_wait: the same fake gh, one push.N.json per call (the last repeats). A "none" is waited out
  # (a --push release), anything else is answered at once, and the time limit ends the wait.
  wait_case() { # wait_case WANT PUSH DESCRIPTION PUSH-JSON...
    local want=$1 push=$2 desc=$3 k=0 f
    shift 3
    rm -f "$tmp/fx/n" "$tmp/fx"/push.[0-9]*.json "$tmp/fx"/tree-*
    for f in "$@"; do k=$((k + 1)); printf '%s' "$f" > "$tmp/fx/push.$k.json"; done
    printf '[]' > "$tmp/fx/mg.json"; rm -f "$tmp/fx/down"
    got=$(FX="$tmp/fx" PATH="$tmp/bin:$PATH" PUSH=$push BIDE_CI_PREFLIGHT_POLL_SECS=0.1 BIDE_CI_PREFLIGHT_WAIT_SECS=1 ci_wait example/r release-sha "$T" 2>/dev/null)
    [ "$got" = "$want" ] || { echo "self-test: $desc: ci_wait said $got, want $want"; ok=false; }
    calls=$(cat "$tmp/fx/n" 2>/dev/null || echo 0)
  }
  local calls
  wait_case success true "no run yet, then going, then green: waited for" '[]' "$running" "$pass"
  [ "$calls" -eq 3 ] || { echo "self-test: ci_wait asked $calls times, want 3"; ok=false; }
  wait_case failure true "no run yet, then failed: stopped at once" '[]' "$fail" "$pass"
  [ "$calls" -eq 2 ] || { echo "self-test: ci_wait asked $calls times after a failure, want 2"; ok=false; }
  wait_case none true "no run ever appears: gives up at the time limit" '[]'
  [ "$calls" -ge 5 ] || { echo "self-test: ci_wait gave up after $calls asks, want a bounded wait of 10"; ok=false; }
  wait_case none true "a run that never completes: gives up at the time limit" "$running"
  wait_case none false "a dry run does not wait" '[]'
  [ "$calls" -eq 1 ] || { echo "self-test: a dry run asked $calls times, want 1"; ok=false; }
  wait_case success true "green at once: no wait" "$pass"
  [ "$calls" -eq 1 ] || { echo "self-test: a green run asked $calls times, want 1"; ok=false; }
  # Documentation-only release commits: CI skips the Go jobs for a push of one such commit, so its
  # own push run is green whatever the code below it did. The commit whose run counts is the first
  # one, walking back, that changes code. Real commits in a scratch repository (docs/ and README.md
  # are documentation, agent/ is code); every commit has a push run, the red code commits a failed one.
  local gr="$tmp/docs-repo" g=(-c user.name=t -c user.email=t@example.com -c commit.gpgsign=false) green_code red_code d_green d_red1 d_red2 d_none code_none
  git init --quiet "$gr"
  mk() { # mk FILE: commit a change to FILE and print the commit
    mkdir -p "$gr/$(dirname "$1")"; echo "$RANDOM" > "$gr/$1"
    git -C "$gr" add -A; git -C "$gr" "${g[@]}" commit --quiet -m "$1"; git -C "$gr" rev-parse HEAD
  }
  mk README.md >/dev/null
  green_code=$(mk agent/a.go); d_green=$(mk docs/a.md)
  red_code=$(mk agent/b.go);   d_red1=$(mk docs/b.md); d_red2=$(mk CHANGELOG.md)
  code_none=$(mk agent/c.go);  d_none=$(mk docs/c.md)
  rm -f "$tmp/fx/n" "$tmp/fx"/push.[0-9]*.json "$tmp/fx"/run-*.json "$tmp/fx/down"
  for c in "$green_code" "$d_green" "$d_red1" "$d_red2" "$d_none"; do printf '%s' "$pass" > "$tmp/fx/run-$c.json"; done
  printf '%s' "$fail" > "$tmp/fx/run-$red_code.json"; printf '[]' > "$tmp/fx/run-$code_none.json"
  printf '[]' > "$tmp/fx/mg.json"
  docs_case() { # docs_case WANT SHA DESCRIPTION
    got=$(REPO=$gr FX="$tmp/fx" PATH="$tmp/bin:$PATH" push_ci_state example/r "$2" 2>/dev/null)
    [ "$got" = "$1" ] || { echo "self-test: $3: push_ci_state said $got, want $1"; ok=false; }
  }
  docs_case success "$green_code" "a code commit with a green run (unchanged)"
  docs_case failure "$red_code" "a code commit with a failed run (unchanged)"
  docs_case success "$d_green" "a documentation commit on a green code commit"
  docs_case failure "$d_red1" "a documentation commit on a red code commit (its own run is green)"
  docs_case failure "$d_red2" "two documentation commits on a red code commit"
  docs_case none "$d_none" "a documentation commit on a code commit with no push run"
  docs_case none "$code_none" "a code commit with no push run"
  got=$(REPO=$gr FX="$tmp/fx" PATH="$tmp/bin:$PATH" BIDE_CI_POLL_SECS=0.1 BIDE_CI_WAIT_SECS=1 root_tag_gate example/r "$d_red2" success >/dev/null 2>&1 && echo go || echo stop)
  [ "$got" = stop ] || { echo "self-test: root_tag_gate let a documentation commit on a red code commit through"; ok=false; }
  got=$(REPO=$gr FX="$tmp/fx" PATH="$tmp/bin:$PATH" BIDE_CI_POLL_SECS=0.1 BIDE_CI_WAIT_SECS=1 root_tag_gate example/r "$d_green" success >/dev/null 2>&1 && echo go || echo stop)
  [ "$got" = go ] || { echo "self-test: root_tag_gate stopped a documentation commit on a green code commit"; ok=false; }
  # The gate comes before the root tag is created and pushed (the self-test cannot run a release).
  local gate_line tag_line
  gate_line=$(grep -n 'root_tag_gate "\$GITHUB_REPO"' "${BASH_SOURCE[0]}" | head -1 | cut -d: -f1)
  tag_line=$(grep -n 'git -C "\$CLONE" tag -a "\$VERSION"' "${BASH_SOURCE[0]}" | head -1 | cut -d: -f1)
  { [ -n "$gate_line" ] && [ -n "$tag_line" ] && [ "$gate_line" -lt "$tag_line" ]; } || { echo "self-test: the root tag gate does not come before the root tag"; ok=false; }
  # The release branch reaches the CI checks and the gate, and the gate that decides it runs in the preflight.
  grep -q 'ci_wait "\$GITHUB_REPO" "\$SHA" .* "\$REL_BRANCH")' "${BASH_SOURCE[0]}" || { echo "self-test: the preflight CI check does not pass the release branch"; ok=false; }
  grep -q 'root_tag_gate "\$GITHUB_REPO" "\$SHA" "\$CI_BASIS" "\$REL_BRANCH"' "${BASH_SOURCE[0]}" || { echo "self-test: the root tag gate does not get the release branch"; ok=false; }
  grep -q 'if release_branch_gate "\$VERSION" "\$SHA"; then' "${BASH_SOURCE[0]}" || { echo "self-test: the preflight does not run the release branch gate"; ok=false; }
  self_test_release_branch "$tmp" || ok=false
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

# self_test_proxy: proxy_wait_resolve (scripts/proxy-wait.sh) on a fake clock and fake go commands.
# The proxy is never asked before the tag is fetchable directly or before the settle delay has
# passed; its waits grow 5, 10, 15, 15 minutes; it gives up at the stated maximum; and the dry run
# (no push) asks once.
self_test_proxy() {
  local ok=true
  (
    set +e
    # A fake clock: sleeping advances it.
    CLOCK=1000; LOG=""; DIRECT_OK_AT=0; PROXY_OK_AT=0
    fake_now() { echo "$CLOCK"; }
    fake_sleep() { CLOCK=$((CLOCK + $1)); SLEPT="$SLEPT $1"; }
    pw_direct() { LOG="$LOG d@$CLOCK"; [ "$CLOCK" -ge "$DIRECT_OK_AT" ]; }
    pw_proxy() { LOG="$LOG p@$CLOCK"; [ "$CLOCK" -ge "$PROXY_OK_AT" ]; }
    pw_log() { :; }
    export PW_SLEEP=fake_sleep PW_NOW=fake_now PW_SETTLE=300 PW_STEP=300 PW_MAX=5400 PW_DIRECT_POLL=15 PW_DIRECT_MAX=900
    run() { CLOCK=1000; LOG=""; SLEPT=""; proxy_wait_resolve "$@" >/dev/null; }
    # Fetchable at once, pushed 10 seconds ago: the proxy waits out the settle delay, then answers.
    DIRECT_OK_AT=0 PROXY_OK_AT=0
    run m v1 990 || { echo "self-test: a version that resolves was not accepted"; exit 1; }
    [ "$LOG" = " d@1000 p@1290" ] || { echo "self-test: expected the direct check at 1000 and the proxy at 1290 (990+300), got [$LOG ]"; exit 1; }
    # Pushed long ago: the proxy is asked at once.
    run m v1 100 || exit 1
    [ "$LOG" = " d@1000 p@1000" ] || { echo "self-test: an old tag was not asked at once: [$LOG ]"; exit 1; }
    # The tag becomes fetchable after 60 seconds: the proxy is not asked before that, nor before the
    # settle delay that starts when it appears.
    DIRECT_OK_AT=1060 PROXY_OK_AT=0
    run m v1 "" || exit 1
    case "$LOG" in " d@1000 d@1015 d@1030 d@1045 d@1060 p@"*) ;; *) echo "self-test: the direct check did not poll to the tag: [$LOG ]"; exit 1 ;; esac
    case "$LOG" in *" p@1360") ;; *) echo "self-test: the proxy was not asked a settle delay (300s) after the tag appeared: [$LOG ]"; exit 1 ;; esac
    # The proxy says no four times: waits of 5, 10, 15 and 15 minutes, then it answers.
    DIRECT_OK_AT=0 PROXY_OK_AT=$((1000 + 300 + 300 + 600 + 900 + 900))
    run m v1 1000 || { echo "self-test: a proxy that answers after four misses was given up on"; exit 1; }
    [ "$SLEPT" = " 300 300 600 900 900" ] || { echo "self-test: the waits were [$SLEPT ], want the settle then 300 600 900 900"; exit 1; }
    # It never answers: it gives up within the maximum, having asked only a few times.
    PROXY_OK_AT=999999999
    if run m v1 1000; then echo "self-test: a proxy that never answers was accepted"; exit 1; fi
    [ $((CLOCK - 1000)) -le $((300 + 5400)) ] || { echo "self-test: waited $((CLOCK - 1000))s, over the settle plus the 5400s maximum"; exit 1; }
    [ "$(echo "$LOG" | tr ' ' '\n' | grep -c '^p@')" -le 8 ] || { echo "self-test: the proxy was asked too often: [$LOG ]"; exit 1; }
    # A resumed run (no push time) does not ask the proxy within the settle delay of its own start.
    DIRECT_OK_AT=0 PROXY_OK_AT=0
    PW_FLOOR=1000 run m v1 "" || exit 1
    [ "$LOG" = " d@1000 p@1300" ] || { echo "self-test: a run with no push time asked the proxy before PW_FLOOR+settle: [$LOG ]"; exit 1; }
    # One deadline bounds the whole wait: a tag fetchable after 200 minutes and a proxy that never
    # answers does not wait 200 + 5 + 90 minutes against a 240 minute allowance.
    DIRECT_OK_AT=$((1000 + 200 * 60)) PROXY_OK_AT=999999999
    if PW_DEADLINE=$((1000 + 240 * 60)) PW_DIRECT_MAX=$((240 * 60)) PW_MAX=$((240 * 60)) run m v1 ""; then echo "self-test: an unanswering proxy was accepted"; exit 1; fi
    [ $((CLOCK - 1000)) -le $((240 * 60)) ] || { echo "self-test: waited $((CLOCK - 1000))s against a deadline of $((240 * 60))s"; exit 1; }
    # The deadline also cuts the settle delay and the direct wait short; the proxy is not asked.
    DIRECT_OK_AT=0 PROXY_OK_AT=0
    if PW_DEADLINE=1100 run m v1 1000; then echo "self-test: a settle delay past the deadline was waited out"; exit 1; fi
    echo "$LOG" | grep -q 'p@' && { echo "self-test: the proxy was asked past the deadline: [$LOG ]"; exit 1; }
    DIRECT_OK_AT=999999999
    if PW_DEADLINE=1100 PW_DIRECT_MAX=99999 run m v1 ""; then echo "self-test: an unfetchable tag was accepted"; exit 1; fi
    [ $((CLOCK - 1000)) -le 130 ] || { echo "self-test: the direct wait ran $((CLOCK - 1000))s past a 100s deadline"; exit 1; }
    # A tag that appears 10 minutes into the run: the settle counts from when it appeared, not from
    # the run's start (already more than the settle delay ago).
    DIRECT_OK_AT=1600 PROXY_OK_AT=0
    PW_FLOOR=1000 PW_DIRECT_MAX=900 run m v1 "" || exit 1
    case "$LOG" in *" p@1900") ;; *) echo "self-test: the proxy was not asked a settle delay after the tag appeared (run start 1000, tag at 1600): [$LOG ]"; exit 1 ;; esac
    # A tag that is never fetchable is not an answer from the proxy: the proxy is never asked.
    DIRECT_OK_AT=999999999
    if run m v1 1000; then echo "self-test: an unfetchable tag was accepted"; exit 1; fi
    echo "$LOG" | grep -q 'p@' && { echo "self-test: the proxy was asked about a tag that is not fetchable: [$LOG ]"; exit 1; }
    exit 0
  ) || ok=false
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
  [ "$(PUBLISHED="a cmd/x b" COMMANDS="cmd/x" "$0" --list-commands 2>/dev/null | tr '\n' ' ')" = "cmd/bide cmd/bide/bide-hsm cmd/bide-anchor " ] ||
    { echo "self-test: --list-commands does not print the commands"; fail=true; }
  local v
  for v in v0.21.0 v0.21.0-rc.1 v0.21.0-rc.10 v0.21.0-beta v0.21.0-0.3.7 v0.21.0-x-y.z; do
    is_version "$v" || { echo "self-test: $v was refused as a version"; fail=true; }
  done
  for v in v0.21.0-rc..1 v0.21.0--. v0.21.0-01 v0.21.0-rc.01 v0.21.0-rc. v0.21.0- v0.21.0+b v01.0.0 v1.02.3 0.21.0 "v0.21.0 "; do
    is_version "$v" && { echo "self-test: \"$v\" was accepted as a version"; fail=true; }
  done
  local cl=$'# Changelog\n\n## [Unreleased]\n\n## [0.21.0-rc.1] - 2026-10-19\n\n## [0.20.0] - 2026-10-16\n'
  [ -n "$(candidate_before "$cl" v0.21.0)" ] || { echo "self-test: a final after its candidate was not warned about"; fail=true; }
  [ -z "$(candidate_before "$cl" v0.21.0-rc.2)" ] || { echo "self-test: a second candidate was warned about"; fail=true; }
  [ -z "$(candidate_before "$cl" v0.22.0)" ] || { echo "self-test: another version was warned about"; fail=true; }
  [ -z "$(candidate_before $'# Changelog\n\n## [Unreleased]\n\n## [0.20.0] - 2026-10-16\n' v0.21.0)" ] || { echo "self-test: a final after a final was warned about"; fail=true; }
  self_test_work || fail=true
  self_test_push || fail=true
  self_test_ci || fail=true
  self_test_changelog || fail=true
  self_test_proxy || fail=true
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
REL_BRANCH=""   # set by release_branch_gate when SHA is released from a release branch
SHA=$(git -C "$REPO" rev-parse --verify "$REF^{commit}")
WORK=$(mktemp -d "${TMPDIR:-/tmp}/bide-release.XXXXXX")
work_traps

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
  *)
    if release_branch_gate "$VERSION" "$SHA"; then
      note "$SHA is the tip of $REMOTE/$REL_BRANCH, a PR-only release branch with required status checks (approvals are not checked: this repository has one maintainer), and $VERSION is the next patch of its line; the branch's own CI decides"
    else
      problem "$SHA is not $REMOTE/main"
    fi ;;
esac
CI_BASIS=none
CI_WHERE=main
[ -z "$REL_BRANCH" ] || CI_WHERE=$REL_BRANCH
if command -v gh >/dev/null 2>&1; then
  ci=$(ci_wait "$GITHUB_REPO" "$SHA" "$(git -C "$REPO" rev-parse "$SHA^{tree}" 2>/dev/null || true)" "$REL_BRANCH")
  case "$ci" in
    success) note "CI on $CI_WHERE is green for $SHA" ;;
    queue) note "no completed CI run on main for $SHA yet, but the merge queue's run of the same tree passed: the preflight and a dry run go on; a --push release waits for main's push run to pass before it tags the root module" ;;
    *) if [ -n "$REL_BRANCH" ]; then problem "CI for $SHA on $REL_BRANCH is $ci, not green"
       else problem "CI for $SHA on main is $ci, not green (and no merge queue run of the same tree passed)"; fi ;;
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
scratch_clone "$REPO" "$CLONE" "$SHA"
mkdir -p "$WORK/modcache" "$WORK/directcache" "$WORK/empty"
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

# pw_direct and pw_proxy are what proxy_wait_resolve (scripts/proxy-wait.sh) asks: whether the tag is
# fetchable from the repository without the proxy or the checksum database hearing of it (a miss
# there is cached for 30 to 60 minutes), and whether the proxy serves it.
pw_direct() {
  (cd "$WORK/empty" && env GOWORK=off GOFLAGS=-mod=mod GOMODCACHE="$WORK/directcache" GOPROXY=direct GOSUMDB=off GOPRIVATE= GONOPROXY= GONOSUMDB= GOINSECURE= go list -m "$1")
}
pw_proxy() { (cd "$WORK/empty" && gorun list -m "$1"); }

# push_epoch: when the last tags were pushed; wait_resolve does not ask the proxy for them sooner.
PUSHED_AT=""
RUN_START=$(date +%s)

# wait_resolve module version: the module proxy (or, in a dry run, the scratch origin) serves it.
# After a push the tag is first checked without the proxy and the proxy is not asked until it has had
# time to settle, then asked with growing waits (scripts/proxy-wait.sh).
wait_resolve() {
  local mod=$1 ver=$2
  if $PUSH; then
    note "waiting for $mod@$ver to resolve on $PROXY (a ${PW_SETTLE:-300}s settle since the push, then up to ${PW_MAX:-5400}s of asking, in all for the stage's modules)"
    # One budget per push (the stage's tags share it): the settle delay and the asking, from the push; a
    # resumed run counts from its own start.
    if PW_FLOOR=$RUN_START PW_DEADLINE=$(( ${PUSHED_AT:-$RUN_START} + ${PW_SETTLE:-300} + ${PW_MAX:-5400} )) proxy_wait_resolve "$mod" "$ver" "$PUSHED_AT"; then note "resolves: $mod@$ver"; return 0; fi
    (cd "$WORK/empty" && gorun list -m "$mod@$ver") || true
    die "$mod@$ver does not resolve; re-run the same command to resume, once the proxy serves it"
  fi
  if (cd "$WORK/empty" && gorun list -m "$mod@$ver" >/dev/null 2>&1); then note "resolves: $mod@$ver"; return 0; fi
  (cd "$WORK/empty" && gorun list -m "$mod@$ver") || true
  die "$mod@$ver does not resolve"
}

# ---- 1. the root tag -------------------------------------------------------------------------
say "root: $ROOT_MODULE $VERSION"
if ! $ROOT_TAGGED; then
  # The one step that cannot be undone: on a queue-only basis, main's push run must pass first.
  if $PUSH; then root_tag_gate "$GITHUB_REPO" "$SHA" "$CI_BASIS" "$REL_BRANCH" || die "not tagging $VERSION"; fi
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
