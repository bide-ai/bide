Section: Changed

`bide demo` now opens with the same crash without bide, where a naive retry charges the fake provider twice (`provider charges: 2`), and ends on the bide run's single charge. On the halt it tells ops with a signed webhook (the real `agent.WithNotifier` and `notify` webhook) delivered to an in-process HTTPS receiver that verifies the signature, and after the proofs it changes one amount in a copy of the journal and shows the real verifier failing on it. `assets/crash-demo.gif` is re-recorded with the `bide view` page live from the first frame.
