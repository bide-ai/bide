# Branch protection (ruleset not applied)

`strict-protection.json` is the `main`-branch ruleset copied verbatim from
`blackwell-systems/gcf` (block deletion, block non-fast-forward, require linear
history; admin role bypasses). It is **not applied**: the repository has one
ruleset, `release branches` (on `release/**`, see CONTRIBUTING, Releasing), and none for `main`. `main` is protected by classic branch protection instead, which
requires the Lint, Test (ubuntu-latest, macos-latest, windows-latest), dco,
Integration (Postgres, Redis), Models, gsm machine gate, Changelog fragment, Docs build and Viewer (Chrome) checks, blocks force pushes and deletion, and merges
through a merge queue (squash). Test (ubuntu-latest), Test (windows-latest) and Models each
stand for parallel shard jobs (`Test (ubuntu-latest, <shard>)`, `Test (windows-latest, <shard>)`,
`Models (<shard>)`): a final job of that name needs every shard
and fails unless each succeeded, or nothing needed checking (`.github/scripts/shards.sh verdict`).
The shard jobs are not required checks themselves, so the shard layout can change without changing
branch protection. On a pull request, Test (macos-latest), Test (windows-latest) and Integration
(Postgres, Redis) are skipped jobs, which satisfy the requirement (Windows runs on a pull request that
changes Windows-sensitive files); they run in the merge queue and on main, except that a documentation-only
push or queue batch skips them too
(see CONTRIBUTING.md, "What CI runs, and when").

`gsm machine gate` (the job of `.github/workflows/gsm-gate.yml`, see `.github/gsm-gate/README.md`)
is required too. Its name is
fixed, and it always runs and reports: it runs on `pull_request`, `merge_group` and pushes to
`main`, with no path filter and no job-level condition, so a documentation-only change runs it as
well (the coverage scan reads the docs) and it never waits as pending.

To apply the ruleset:

```sh
gh api --method POST repos/bide-ai/bide/rulesets \
  --input .github/rulesets/strict-protection.json
```

Verify:

```sh
gh api repos/bide-ai/bide/rulesets
```
