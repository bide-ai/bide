Section: Changed

A live model turn whose tool-use ID spells the escalation instance of a plan flow node (`node:<name>:taint`, `node:iter:<n>:<name>:taint`) now fails with `ErrToolUseIDInvalid`, as an ID that holds `#` does; no other ID is affected.
