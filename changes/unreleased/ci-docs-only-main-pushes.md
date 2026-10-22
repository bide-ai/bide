Section: Changed

CI: a push to main that is one commit changing only documentation or the changelog now skips the Go test jobs, as a pull request does (Lint, the docs checks and the changelog check still run, and the CI run still completes with success, so the release script's gates read it as before). The commit is diffed against the push's before SHA; a push of several commits, a force push or a first push runs everything.
