Section: Added

`plan.Untrusted`, `plan.Trusted`, `plan.TaintExempt`, `Builder.WithTaintPolicy`, `plan.WithLoadedTaintPolicy` and the `untrusted` key of a flow config node exist, but flow taint is not enforced yet: `Build`, `Load` and `bide check` refuse every flow that uses any of them, with `ErrConfig` ("flow taint is not enforced yet"), rather than run it with no taint handling; `Validate` accepts the `untrusted` key, as it does not run `Build`'s checks. The key is refused on an edge or a switch element. Experimental.
