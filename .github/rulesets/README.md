# Branch protection (deferred)

`strict-protection.json` is the `main`-branch ruleset copied verbatim from
`blackwell-systems/gcf` (block deletion, block non-fast-forward, require linear
history; admin role bypasses). It is **not applied yet**: this repo is private on
a free plan, and GitHub blocks rulesets / branch protection on private repos.

Apply it the moment the repo becomes public **or** the account upgrades to
GitHub Pro:

```sh
gh api --method POST repos/blackwell-systems/go-agents/rulesets \
  --input .github/rulesets/strict-protection.json
```

Verify:

```sh
gh api repos/blackwell-systems/go-agents/rulesets
```
