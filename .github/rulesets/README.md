# Branch protection (ruleset not applied)

`strict-protection.json` is the `main`-branch ruleset copied verbatim from
`blackwell-systems/gcf` (block deletion, block non-fast-forward, require linear
history; admin role bypasses). It is **not applied**: the repository has no
rulesets. `main` is protected by classic branch protection instead, which
requires the Lint, Test (ubuntu-latest, macos-latest, windows-latest), dco and
Integration (Postgres, Redis) checks, blocks force pushes and deletion, and merges through a merge queue
(squash).

To apply the ruleset:

```sh
gh api --method POST repos/bide-ai/bide/rulesets \
  --input .github/rulesets/strict-protection.json
```

Verify:

```sh
gh api repos/bide-ai/bide/rulesets
```
