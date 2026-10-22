Section: Changed

CI: Test (windows-latest) now also runs on a pull request that changes a test file, testdata or a golden file, .gitattributes, a file that is not Go or documentation (go.mod, scripts, workflows), or a Go file that imports os, path, path/filepath, io/fs, syscall, mime, encoding or golang.org/x/sys, so Windows-only failures (line endings, MIME types, file identity, signals) show before the merge queue. Any other pull request still skips it, which satisfies the required check; the merge queue and main run it on every change as before.
