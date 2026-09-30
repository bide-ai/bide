## What

<!-- What changes, and why. -->

## API and behavior changes

<!-- Breaking changes, journal or audit format changes, new exports. "None" if none. -->

## Verification

<!-- Failing test first, mutation check, and the commands you ran. -->

## Checklist

- [ ] Commits are signed off (`git commit -s`, see DCO)
- [ ] `GOWORK=off go vet ./...` and `GOWORK=off go test ./...` in every module touched, toolchain `gofmt -l .`, and `go run ./internal/tools/doccheck -root . -allow .doccheck-allow` are clean
- [ ] CHANGELOG.md updated under Unreleased (or not user-facing)
- [ ] Docs updated for any user-visible change
