package gcs

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// The custom IAM roles the anchor needs, as the JSON that "gcloud iam roles create --file" reads.
// WriterRole is for the application that publishes heads, ReaderRole for an auditor or monitor that
// reads them, and HoldReleaserRole for the separate identity that releases a temporary hold (needed
// only with Config.LegalHold). Bind each at the bucket, not the project.
//
// WriterObjectRetentionRole is the writer role for Config.ObjectRetention: it adds
// storage.objects.setRetention, and nothing else.
//
// None of the others grants storage.objects.delete (so the writer cannot overwrite an object either: replacing one
// needs both create and delete), storage.objects.setRetention, storage.objects.overrideUnlockedRetention,
// storage.buckets.update, any setIamPolicy permission or any permission that locks or changes the bucket.
// Grant those, if anyone must hold them, to an administrator outside the application's operators.
//
// A test checks that every Store method is allowed by the role that uses it (MethodPermissions), so a
// call cannot be added without the role being reviewed.
var (
	// WriterRole is the role for the application that publishes heads.
	//
	//go:embed iam/writer.json
	WriterRole string
	// WriterObjectRetentionRole is WriterRole plus storage.objects.setRetention, which a create that sets
	// a per-object retention needs (Config.ObjectRetention); verified against a real bucket, where the
	// plain writer role was refused with a 403 naming that permission. Use it only with ObjectRetention.
	//
	//go:embed iam/writer-object-retention.json
	WriterObjectRetentionRole string
	// ReaderRole is the role for an auditor or monitor that only reads heads.
	//
	//go:embed iam/reader.json
	ReaderRole string
	// HoldReleaserRole is the role for the identity that releases temporary holds.
	//
	//go:embed iam/hold-releaser.json
	HoldReleaserRole string
)

// ObjectRetentionPermissions are the permissions a Create that sets a per-object retention needs in
// addition to MethodPermissions["Create"].
var ObjectRetentionPermissions = []string{"storage.objects.setRetention"}

// RolePermissions returns the permissions a role document (WriterRole and the others) grants.
func RolePermissions(role string) ([]string, error) {
	var r struct {
		IncludedPermissions []string `json:"includedPermissions"`
	}
	if err := json.Unmarshal([]byte(role), &r); err != nil {
		return nil, fmt.Errorf("gcs: role document: %w", err)
	}
	return r.IncludedPermissions, nil
}
