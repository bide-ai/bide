Section: Fixed

`bide doctor` failed on a Go older than 1.27 even where `GOTOOLCHAIN=auto` (the default) lets go fetch 1.27 itself inside a bide project. It is now a warning there, and still a failure under `GOTOOLCHAIN=local` or a pinned older toolchain. The fix text names `GOTOOLCHAIN=go1.27.0` as the getting-started guide does, long lines wrap so the output fits 120 columns, and in color the check names line up whether a row has a mark or not.
