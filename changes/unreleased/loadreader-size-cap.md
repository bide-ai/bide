Section: Changed

`plan.LoadReader` reads at most 16 MiB and refuses a larger config with an error wrapping `agent.ErrConfig`, without reading past the byte that shows it is too large. It used to read its reader to the end however long that was, so a reader that never ended (an HTTP body, a pipe) filled memory. No real config is near the limit; to load a larger one, read it yourself and call `plan.Load`.

The viewer's config readers (`bide view -config`, `-diff`, and the checks that share them) apply the same 16 MiB limit to a flow config file or a `-diff` git revision; they allowed 256 MiB before. `bide edit` already refuses a config over 1 MiB.
