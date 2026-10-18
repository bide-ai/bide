package gcs

import "github.com/bide-ai/bide/audit/anchorobj"

// suspectID returns a reason when a run or trail ID looks like it holds personal data, or "": the
// check shared with the other anchors (anchorobj.SuspectID), which says what it reports and what it
// does not.
func suspectID(id string) string { return anchorobj.SuspectID(id) }
