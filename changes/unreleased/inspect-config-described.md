Section: Added

`plan.InspectConfigDescribed` checks a flow config against a `RegistryDescription` instead of a Registry, with the same code `InspectConfig` runs: blocks, predicates, merges, types, safety, approval gates and the digest are checked without the program that registered them. `RegistryDescription.Validate` reports why a description read from a file is not one this release reads. Types are told apart by name only, so two distinct types spelled the same are one type, and messages write a type with its package path. Experimental: may change in any 0.x release.
