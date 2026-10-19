Section: Changed

gcs.LoadDir now uses the directory loader shared in audit/anchorobj (Layout.LoadDir) instead of its own copy: no change in behaviour (an empty directory, equivocation in one partition, a stray file, a symbolic link and an oversized file are still errors), and, as awss3.LoadDir already does, a path that is not a directory is an error wrapping ErrObject.
