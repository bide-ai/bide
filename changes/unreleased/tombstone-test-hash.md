Section: Fixed

Test only: `TestNewTombstone_ReadsBack` built its changed-hash case by replacing the first two hex digits of the leaf hash with "00", which changed nothing when the hash already began with "00" (about one run in 256, since the record carries the time it was made), so the test failed at random. It now flips the first byte.
