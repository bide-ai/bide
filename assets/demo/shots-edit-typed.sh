#!/usr/bin/env bash
# shots-edit-typed.sh: regenerate the flow editor guide's pictures of typed mode: docs/guides/img/flow-edit-typed,
# flow-edit-typed-blocks, flow-edit-typed-uncertified and flow-edit-typed-save (light, 1280 wide): bide edit
# -blocks over a refund flow committed in a throwaway git repository, with the blocks description a program's
# plan.Registry writes (setup/edit-blocks.go). The page is real and nothing is mocked; browser-shots/edit-typed-shots.mjs
# drives it. Like the guide's other editor pictures they are light, and the file names carry no theme.
#
#   assets/demo/shots-edit-typed.sh                     # into docs/guides/img
#   OUT=/tmp/shots assets/demo/shots-edit-typed.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=light
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8681}
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
require_free "$port"
shot_init

(cd "$root/cmd/bide" && GOWORK=off go build -o "$work/bide" .)
(cd "$root/cmd/bide" && GOWORK=off go run ../../assets/demo/setup/edit-blocks.go -out "$work/blocks.json")
mkdir -p "$out" "$work/repo"
cd "$work/repo"
git init -q .
git config user.email shots@example.invalid
git config user.name shots
cat > refund.yaml <<'YAML'
version: 1
flow: refund
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - name: charge
    block: charge
    safety: side_effect
    approval:
      need: 2
      approvers: [ops, finance, risk]
      exclude_principal: true
      timeout: 24h
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, charge]
  - edge: [charge, receipt]
assert:
  - every path to charge passes approval >= 2 of 3
YAML
git add refund.yaml
git commit -q -m "the refund flow"
"$work/bide" edit -addr "127.0.0.1:$port" -blocks "$work/blocks.json" refund.yaml > "$work/edit.log" 2>&1 &
url=
for _ in $(seq 300); do
  url=$(grep -o 'http://[^ ]*' "$work/edit.log" | head -1 || true)
  [ -n "$url" ] && break
  sleep 0.1
done
[ -n "$url" ] || { echo "shots-edit-typed.sh: bide edit printed no address" >&2; cat "$work/edit.log" >&2; exit 1; }
(cd "$(shot_dir)" && node edit-typed-shots.mjs "$url" "$out")
for n in flow-edit-typed flow-edit-typed-blocks flow-edit-typed-uncertified flow-edit-typed-save; do echo "$out/$n.png"; done
