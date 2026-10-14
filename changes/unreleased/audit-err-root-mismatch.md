Section: Added

`audit.ErrRootMismatch` is a new sentinel wrapped by the error of a proof whose signed tree head's root is not the root of the run's journal (the message is unchanged); `bide demo` uses it to explain a failed verification in plain words.
