Section: Changed

CI: Test (windows-latest) is now skipped on every run that changes no code, not only on a pull request: a documentation-only push to main (and a documentation-only merge queue batch) no longer starts the Windows job that the Go jobs' skip was meant to save. The decision moved into `.github/scripts/windows-decide.sh`, whose self-test runs in CI and covers each event and a failed diff. A skipped job still satisfies the required check, and any doubt still runs it.
