Section: Fixed

`bide view` says why a gate's signatures were not checked when the `-sth` or `-evidence` given is for another run: `signatures not checked: the proof given is for another run (signed head: "run-b"); give -sth or -evidence for this run`. It used to say `pass -approver-keys, with -sth or -evidence`, which a user who had passed all three could not act on. The run IDs are shown escaped. A `runview.Prover` of your own that returns nil keeps the old line. Nothing about what is checked changes: a run without a proof is still unchecked.
