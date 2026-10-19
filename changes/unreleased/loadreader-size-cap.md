Section: Changed

`plan.LoadReader` reads at most 16 MiB and refuses a larger config with an error wrapping `agent.ErrConfig`, without reading past the byte that shows it is too large. It used to read its reader to the end however long that was, so a reader that never ended (an HTTP body, a pipe) filled memory. No real config is near the limit; to load a larger one, read it yourself and call `plan.Load`.
