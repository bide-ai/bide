Section: Changed

A live model turn whose tool-use ID starts with `node:` now fails with `ErrToolUseIDInvalid`, as an ID that holds `#` does: plan flows key their nodes and the gates on them by `node:`, and a node's escalation instance is `node:<name>:taint`. A journal that already holds such an ID still replays and resumes; no other ID is affected.
