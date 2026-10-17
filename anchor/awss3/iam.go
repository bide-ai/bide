package awss3

import (
	_ "embed"
	"strconv"
	"strings"
)

// The IAM policies the anchor needs, as templates. WriterPolicy is for the application that
// publishes heads, ReaderPolicy for an auditor or monitor that reads them, and BucketPolicy is the
// bucket policy that denies everyone (the account's administrators included, unless they first
// remove the statement, which an SCP in the organization can forbid) the actions that would weaken
// the anchor. None grants s3:DeleteObject, s3:DeleteObjectVersion or s3:BypassGovernanceRetention.
//
// Replace the placeholders with PolicyFor. The bucket policy also denies a write whose lock mode is not the
// configured one, whose retention is under the floor, or whose KMS key is not the configured key, so
// a writer (which may be an adversarial operator) cannot anchor a head with a weaker lock than
// Config says. Grant s3:PutObjectLegalHold, and s3:GetObjectLegalHold
// to the reader, only if Config.LegalHold is set, and to a separate identity from the writer's if you want the
// hold to be releasable by someone other than the application.
var (
	// WriterPolicy is the policy template for the application that publishes heads.
	//
	//go:embed iam/writer.json
	WriterPolicy string
	// ReaderPolicy is the policy template for an auditor or monitor that only reads heads.
	//
	//go:embed iam/reader.json
	ReaderPolicy string
	// BucketPolicy is the bucket policy template that denies deletes, governance bypass and weaker writes.
	//
	//go:embed iam/bucket.json
	BucketPolicy string
)

// PolicyParams fills in a policy template.
type PolicyParams struct {
	Bucket    string // the anchor bucket
	Prefix    string // the key prefix as configured, with its trailing slash ("bide-anchor/" when empty)
	KMSKeyARN string // the ARN of the KMS key heads are encrypted with
	// LockMode is the object lock mode every head must carry: "COMPLIANCE" (the default) or "GOVERNANCE".
	LockMode string
	// MinRetentionDays is the floor of the retention a write may ask for: the days of Config.Retention,
	// rounded down. A bucket policy that sets none lets a writer anchor heads with a one-day lock.
	MinRetentionDays int
	// AdminRoleARN is the one principal allowed to change or delete the bucket policy. Put it in the
	// anchor account, outside the application's administrators, and guard it with a service control
	// policy: nothing in a bucket policy stops the account's owner from removing the policy itself.
	AdminRoleARN string
}

// PolicyFor fills a policy template in. Templates use the placeholders BUCKET, PREFIX, KMS_KEY_ARN,
// LOCK_MODE, MIN_RETENTION_DAYS and ADMIN_ROLE_ARN; a template that does not use one ignores it.
func PolicyFor(template string, p PolicyParams) string {
	if p.Prefix == "" {
		p.Prefix = "bide-anchor/"
	}
	if p.LockMode == "" {
		p.LockMode = "COMPLIANCE"
	}
	return strings.NewReplacer("BUCKET", p.Bucket, "PREFIX", p.Prefix, "KMS_KEY_ARN", p.KMSKeyARN, "LOCK_MODE", p.LockMode,
		"MIN_RETENTION_DAYS", strconv.Itoa(p.MinRetentionDays), "ADMIN_ROLE_ARN", p.AdminRoleARN).Replace(template)
}
