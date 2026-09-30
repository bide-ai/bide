#!/usr/bin/env bash
# release.sh: tag a bide release, the root module and every published nested module.
#
#   scripts/release.sh vX.Y.Z [--push] [--ref REF] [--remote NAME] [--no-test] [--keep]
#   scripts/release.sh --check-tag TAG     (CI: the module a nested tag names is releasable)
#   scripts/release.sh --check-classification
#   scripts/release.sh --list-published
#   scripts/release.sh --self-test
#
# On main, every nested module requires github.com/bide-ai/bide v0.0.0 (and govern v0.0.0 for the
# governed-event logs) and resolves it through a replace directive, so the workspace and
# `GOWORK=off` builds use the code in the tree. A consumer cannot resolve v0.0.0, so a nested
# module is released from a commit made for the purpose:
#
#   1. The root module is tagged vX.Y.Z at REF (default HEAD, which must be origin/main with a green
#      CI run), the tag is pushed, and the script waits until the module proxy serves it.
#   2. In stages, each published module whose bide dependencies are already released gets a
#      go.mod that requires them at vX.Y.Z with their replace directives dropped, and a go.sum
#      resolved through the proxy (`go mod tidy`). The module is built, vetted and tested with
#      GOWORK=off against those published versions, one signed-off commit records the stage, each
#      module is tagged <dir>/vX.Y.Z at it, the tags are pushed, and the script waits for the proxy.
#      The first stage holds the modules that need only the root; the governed-event logs, which
#      also need govern, come in the second.
#   3. Every published module is resolved with `go list -m <module>@vX.Y.Z`, and a scratch consumer
#      module that `go get`s all of them at vX.Y.Z builds with a fresh module cache.
#
# The release commits are reachable from their tags only; main keeps its replace directives, so
# development is unchanged. Repo-only modules (examples, integration, benchmarks) are never tagged.
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
PUBLISHED="govern store/sqlite store/postgres mcp trace codec/gcf govern/sqlitelog govern/redislog govern/postgreslog"
# Modules that exist only to keep their dependencies out of the core. They keep their replace
# directives and are never tagged.
REPO_ONLY="examples/approval examples/govern examples/mcp examples/observability examples/plan integration benchmarks"

# gorun runs the go command the way the release resolves modules; the release flow below replaces it.
gorun() { env GOWORK=off go "$@"; }

die() { echo "release: error: $*" >&2; exit 1; }
say() { echo "==> $*"; }
note() { echo "    $*"; }

usage() { sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

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
  $fail && return 1
  echo "self-test: ok"
}

case "${1:-}" in
  --check-tag) [ $# -eq 2 ] || usage; check_tag "$2"; exit ;;
  --check-classification) check_classification "$(git rev-parse --show-toplevel)" && echo "release: every module is classified"; exit ;;
  --list-published) for m in $PUBLISHED; do echo "$m"; done; exit ;;
  --self-test) self_test; exit ;;
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
if [ "$SHA" != "$(git -C "$REPO" rev-parse "$REMOTE/main" 2>/dev/null || true)" ]; then
  problem "$SHA is not $REMOTE/main"
else
  note "$SHA is $REMOTE/main"
fi
if command -v gh >/dev/null 2>&1; then
  ci=$(gh run list -R "$GITHUB_REPO" --workflow ci.yml --commit "$SHA" --event push --json status,conclusion \
    --jq '[.[] | select(.status == "completed")] | map(.conclusion) | if length == 0 then "none" elif all(. == "success") then "success" else "failure" end' 2>/dev/null || echo "unknown")
  if [ "$ci" = "success" ]; then note "CI on main is green for $SHA"; else problem "CI for $SHA on main is $ci, not green"; fi
else
  problem "gh is not installed, so CI status for $SHA cannot be checked"
fi
git -C "$REPO" show "$SHA:docs/releases/$VERSION.md" >/dev/null 2>&1 || problem "docs/releases/$VERSION.md does not exist at $SHA"
git -C "$REPO" show "$SHA:CHANGELOG.md" 2>/dev/null | grep -q "^## \[${VERSION#v}\]" || problem "CHANGELOG.md has no [${VERSION#v}] section at $SHA"
remote_tags=$(git -C "$REPO" ls-remote --tags "$REMOTE" 2>/dev/null || true)
ROOT_TAGGED=false
root_remote=$(echo "$remote_tags" | awk -v t="refs/tags/$VERSION^{}" '$2 == t {print $1}')
[ -n "$root_remote" ] || root_remote=$(echo "$remote_tags" | awk -v t="refs/tags/$VERSION" '$2 == t {print $1}')
if [ -n "$root_remote" ]; then
  [ "$root_remote" = "$SHA" ] || die "$VERSION is already tagged on $REMOTE at $root_remote, not $SHA"
  if $PUSH; then ROOT_TAGGED=true; note "$VERSION is already on $REMOTE at $SHA; resuming"; else problem "$VERSION is already on $REMOTE"; fi
fi
ALREADY=""   # nested modules whose tag is already on the remote (a resumed --push run)
for m in $PUBLISHED; do
  if echo "$remote_tags" | grep -q "refs/tags/$m/$VERSION$"; then
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

push_tags() { # tag...
  local refs="" t
  for t in "$@"; do refs="$refs refs/tags/$t"; done
  # shellcheck disable=SC2086
  git -C "$CLONE" push --quiet "$TARGET" $refs
  if $PUSH; then note "pushed $*"; else note "pushed to the scratch origin: $*"; fi
}

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
    (cd "$CLONE/$m" && go mod edit $edits && gorun mod tidy 2>"$WORK/tidy.log") || { cat "$WORK/tidy.log"; die "$m: go mod tidy failed"; }
    check_gomod "$CLONE/$m"
    (cd "$CLONE/$m" && gorun build ./... && gorun vet ./...)
    if $TEST; then (cd "$CLONE/$m" && gorun test -count=1 ./... >"$WORK/test.log" 2>&1) || { cat "$WORK/test.log"; die "$m: tests failed"; }; fi
    note "$m: requires $(bide_requires "$CLONE/$m" | tr '\n' ' ')at $VERSION; builds$($TEST && echo ", vets and tests" || echo " and vets") with GOWORK=off"
  done
  # shellcheck disable=SC2086
  (cd "$CLONE" && git add -- $(for m in $ready; do echo "$m/go.mod $m/go.sum"; done) \
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
  {
    echo "// Command check imports every published bide module."
    echo "package main"
    echo
    echo "import ("
    echo "	_ \"$ROOT_MODULE/agent\""
    for m in $PUBLISHED; do echo "	_ \"$(module_path "$m")\""; done
    echo ")"
    echo
    echo "func main() {}"
  } > main.go
  # shellcheck disable=SC2046
  gorun get "$ROOT_MODULE@$VERSION" $(for m in $PUBLISHED; do echo "$(module_path "$m")@$VERSION"; done) 2>"$WORK/get.log" || { cat "$WORK/get.log"; die "go get failed"; }
  gorun mod tidy 2>/dev/null
  gorun build ./...
  got=$(gorun list -m -f '{{.Path}} {{.Version}}' all | grep "^$ROOT_MODULE[ /]")
  echo "$got" | sed 's/^/    consumer selects: /'
  wrong=$(echo "$got" | awk -v v="$VERSION" '$2 != v')
  [ -z "$wrong" ] || die "the consumer selected other versions: $wrong"
)
note "a consumer module requiring every published module at $VERSION builds"

if $PUSH; then
  git -C "$REPO" fetch --quiet "$REMOTE" --tags
  say "released $VERSION: the root and $(echo $PUBLISHED | wc -w | tr -d ' ') nested modules resolve on $PROXY"
else
  say "dry run complete: $VERSION and $(echo $PUBLISHED | wc -w | tr -d ' ') nested tags were made in a scratch clone and pushed only to a scratch origin"
  git -C "$CLONE" log --oneline --decorate "$SHA^..HEAD" | sed 's/^/    /'
  note "run again with --push to release"
fi
