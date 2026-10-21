Section: Fixed

Test only: the two anchor/awss3 tests that check what a changelog entry says read CHANGELOG.md once a release has assembled the fragments, instead of skipping; the integration job fails on a skipped test, so main's CI went red on the v0.22.0 release commit.
