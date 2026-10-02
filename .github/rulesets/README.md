# Branch protection (ruleset not applied)

`strict-protection.json` is the `main`-branch ruleset copied verbatim from
`blackwell-systems/gcf` (block deletion, block non-fast-forward, require linear
history; admin role bypasses). It is **not applied**: the repository has no
rulesets. `main` is protected by classic branch protection instead, which
requires the Lint, Test (ubuntu-latest, macos-latest, windows-latest), dco,
Integration (Postgres, Redis) and Models checks, blocks force pushes and deletion, and merges
through a merge queue (squash). Test (ubuntu-latest) and Models each stand for parallel shard jobs
(`Test (ubuntu-latest, <shard>)`, `Models (<shard>)`): a final job of that name needs every shard
and fails unless each succeeded, or nothing needed checking (`.github/scripts/shards.sh verdict`).
The shard jobs are not required checks themselves, so the shard layout can change without changing
branch protection.

To apply the ruleset:

```sh
gh api --method POST repos/bide-ai/bide/rulesets \
  --input .github/rulesets/strict-protection.json
```

Verify:

```sh
gh api repos/bide-ai/bide/rulesets
```
