Section: Fixed

`bide audit verify-evidence` names a step whose key holds a call ID by that ID, as the other verbs show it. The line for the gate of call `c2#taint` read `step "approval-pending:c2%23taint"`, the escaped form the engine stores in the key; it now reads `step "approval-pending" of call "c2#taint"`, and a key suffix stays (`step "approval-tally:v3" of call "c2#taint"`). A key that is not that shape, or whose ID is a digest (an ID over 96 escaped bytes), is shown as stored. Display only: the stored key, the package and its seal are unchanged.
