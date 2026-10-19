Section: Added

`bide check`, `bide view -config` and `bide check -diff` take `-blocks <file>` (and `-old-blocks <file>` with `-diff`): a description of the program's registered blocks, written with `json.Marshal` of `plan.Registry.Describe()`. The check then covers block, predicate and merge names, types, safety, the approval gates blocks declare in Go and the digest, and a block swapped for one that declares no gate is reported as a weakening. The result is labelled as per the description file, not checked against the program.
