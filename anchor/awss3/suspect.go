package awss3

import "github.com/bide-ai/bide/audit/anchorobj"

// suspectID returns a reason when a run or trail ID looks like it holds personal data, or "". The
// anchor is write-once: an object cannot be erased before its retention ends, and a signed head
// carries its run ID in clear (the ID is part of the signed bytes). The check is audit/anchorobj's
// SuspectID, shared with the other anchors; see it for what it reports and what it does not.
func suspectID(id string) string { return anchorobj.SuspectID(id) }
