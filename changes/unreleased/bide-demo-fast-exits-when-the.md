Section: Fixed

`bide demo -fast`, documented as for CI, served the viewer until Ctrl-C. It now exits 0 when the demo is done, without serving or opening a browser; with `-keep` it prints the `bide view` command that reopens the run.
