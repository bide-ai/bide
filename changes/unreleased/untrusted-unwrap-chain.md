Section: Security

A tool that wraps another through `Unwrap` and does not carry an `Untrusted` declaration of the wrapped tool (the option, or a source in `Safety`) is now refused at registration with `ErrConfig`, as one that weakens the wrapped tool's approval gate, timeout or cap rules is (#505). Before, a wrapper that held the inner tool in a named field and wrote its own `Spec` registered with the declaration gone. A wrapper that neither embeds the inner tool nor implements `Unwrap` is still not seen.
