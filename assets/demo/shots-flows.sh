#!/usr/bin/env bash
# shots-flows.sh: regenerate the run viewer guide's screenshots of plan flows:
# docs/guides/img/flow-gate-waiting and flow-gate-approved (examples/approval/flow: a flow paused at
# its refund node's 2 of 3 gate with one approval, then approved twice with the evidence verifying),
# flow-diff-strengthened and flow-diff-weakened (bide check -diff over examples/plan/flows/refund-v1.yaml
# and refund-v2.yaml, both ways) and run-viewer-config-problem (bide view -config over refund-typo.yaml,
# whose last edge names an undeclared node). Each uses the commands the guide lists, and the pages are
# photographed by lib.sh (browser-shots/capture.mjs).
#
#   assets/demo/shots-flows.sh                    # dark, into docs/guides/img
#   THEME=light OUT=/tmp/shots assets/demo/shots-flows.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
theme=${THEME:-dark}
out=${OUT:-$root/docs/guides/img}
port=${PORT:-8661}
work=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null; rm -rf "$work"' EXIT

. "$here/lib.sh"
shot_init

(cd "$root/cmd/bide" && GOWORK=off go build -o "$work/bide" .)
(cd "$root/examples/approval" && GOWORK=off go build -o "$work/flow" ./flow)
mkdir -p "$out"

wait_for() { # url: retry for up to 30 s
  for _ in $(seq 300); do curl -sf -o /dev/null "$1" && return 0; sleep 0.1; done
  echo "shots-flows.sh: timed out waiting for $1" >&2; exit 1
}

# The plan flows of examples/plan, from that directory as the guide runs them.
cd "$root/examples/plan"
"$work/bide" view -config flows/refund-typo.yaml -addr "127.0.0.1:$port" > /dev/null &
wait_for "http://127.0.0.1:$port/"
shot run-viewer-config-problem "http://127.0.0.1:$port/" 1100
"$work/bide" check flows/refund-v2.yaml -diff flows/refund-v1.yaml -addr "127.0.0.1:$((port + 1))" > /dev/null &
wait_for "http://127.0.0.1:$((port + 1))/"
shot flow-diff-strengthened "http://127.0.0.1:$((port + 1))/" 1200
"$work/bide" check flows/refund-v1.yaml -diff flows/refund-v2.yaml -addr "127.0.0.1:$((port + 2))" > /dev/null &
wait_for "http://127.0.0.1:$((port + 2))/"
shot flow-diff-weakened "http://127.0.0.1:$((port + 2))/" 1250

# The approval flow, a refund paused at its 2 of 3 gate.
cd "$work"
./flow run -db f.db > /dev/null 2>&1 || true            # the flow pauses before refund
./flow approve -db f.db -as ops > /dev/null
./flow run -db f.db > /dev/null 2>&1 || true            # still paused: 1 of 2 approvals
"$work/bide" view -db f.db -run refund-flow-42 -addr "127.0.0.1:$((port + 3))" > /dev/null &
wait_for "http://127.0.0.1:$((port + 3))/"
shot flow-gate-waiting "http://127.0.0.1:$((port + 3))/run?id=refund-flow-42" 1000
./flow approve -db f.db -as risk > /dev/null
./flow run -db f.db > /dev/null 2>&1                    # the refund runs once; the flow finishes
pub=$(./flow evidence -db f.db -out fev.json 2>&1 | grep -o '[0-9a-f]\{64\}' | head -1)
"$work/bide" view -db f.db -run refund-flow-42 -evidence fev.json -pubkey "$pub" -approver-keys approver-keys.json -addr "127.0.0.1:$((port + 4))" > /dev/null &
wait_for "http://127.0.0.1:$((port + 4))/"
shot flow-gate-approved "http://127.0.0.1:$((port + 4))/run?id=refund-flow-42" 1500
