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
- [ ] Code marked `// protocol:<model>` changed: the model under `spec/tla/<model>/` changes too, or the description says why not (`Protocol-Impact: none (<reason>)`); the model-to-code map in `spec/tla/README.md` still holds for every function touched; a protocol bug found in review is first written as a model configuration and checked
