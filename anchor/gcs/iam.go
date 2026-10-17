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
// None grants storage.objects.delete (so the writer cannot overwrite an object either: replacing one
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
	// ReaderRole is the role for an auditor or monitor that only reads heads.
	//
	//go:embed iam/reader.json
	ReaderRole string
	// HoldReleaserRole is the role for the identity that releases temporary holds.
	//
	//go:embed iam/hold-releaser.json
	HoldReleaserRole string
)

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
