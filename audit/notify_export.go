package audit

import (
	"context"
	"fmt"
	"time"
)

// NotifyTrailExportFormat is the format of a NotifyTrailExport.
const NotifyTrailExportFormat = "bide.audit.notify-trail-export.v1"

// A NotifyTrailExport is a notification trail as the leaves its EventStore holds, in position
// order: the input VerifyNotifications and bide audit notifications read when they hold no store.
// Each leaf is carried verbatim (base64 in JSON), never re-encoded, so a signed head over the trail
// (SignNotifyTrail) commits to exactly the bytes exported.
type NotifyTrailExport struct {
	Format string   `json:"format"` // NotifyTrailExportFormat
	Trail  string   `json:"trail"`  // the trail's name (agent.NotifyTrailName)
	Leaves [][]byte `json:"leaves"` // each leaf, in position order
}

func (NotifyTrailExport) artifactFormat() (string, string) {
	return "notify trail export", NotifyTrailExportFormat
}

// ExportNotifyTrail returns the trail named trail in store as a NotifyTrailExport. A trail the
// store does not hold exports with no leaves.
func ExportNotifyTrail(ctx context.Context, store EventStore, trail string) (NotifyTrailExport, error) {
	leaves, err := store.Load(ctx, trail)
	if err != nil {
		return NotifyTrailExport{}, fmt.Errorf("audit: export notify trail %s: %w", trail, err)
	}
	if leaves == nil {
		leaves = [][]byte{}
	}
	return NotifyTrailExport{Format: NotifyTrailExportFormat, Trail: trail, Leaves: leaves}, nil
}

// SignNotifyTrail signs a tree head over the leaves store holds for trail, at now. The head's Kind
// is TreeEvents and its RunID is the trail's name, so it verifies only as a commitment to that
// trail. Publish it to an Anchor (or any log in another trust domain) as often as the audit needs
// to bound what could be rewritten: VerifyNotifications reads records after the newest head it is
// given as unattested. The notifier does not call this; a deployment signs and anchors on its own
// schedule, as it does for a journal (AuditedStore).
func SignNotifyTrail(ctx context.Context, store EventStore, trail string, s Signer, now time.Time) (SignedTreeHead, error) {
	log, err := LoadEventLog(ctx, store, trail)
	if err != nil {
		return SignedTreeHead{}, err
	}
	return SignTreeHead(log.TreeHead(trail, now.UnixNano()), s)
}
