Section: Added

`Registry.Describe` returns a `RegistryDescription` of what a Registry has registered: each block's name, kind, input and output type names, declared safety and approval gate, each predicate's switched type, each merge's input types, and any registration errors. It is data only (no functions or reflect values), sorted by name, and is the first part of the visual editor's typed mode. Experimental: it may change in any 0.x release.
