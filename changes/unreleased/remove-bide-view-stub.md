Section: Removed

The `cmd/bide-view` module, a stub that only printed that `bide-view` is now `bide view`, is removed and no longer tagged at a release. Its last tag is `cmd/bide-view/v0.25.0`, so `go install github.com/bide-ai/bide/cmd/bide-view@latest` still resolves to that stub, which prints that it moved; install `cmd/bide` (`go install github.com/bide-ai/bide/cmd/bide@latest` or `brew install bide-ai/tap/bide`) and run `bide view`. Nothing else changes: `bide view` is part of `cmd/bide`, and a release tags one module fewer.
