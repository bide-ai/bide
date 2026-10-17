package awss3

import (
	_ "embed"
	"strings"
)

// The IAM policies the anchor needs, as templates. WriterPolicy is for the application that
// publishes heads, ReaderPolicy for an auditor or monitor that reads them, and BucketPolicy is the
// bucket policy that denies everyone (the account's administrators included, unless they first
// remove the statement, which an SCP in the organization can forbid) the actions that would weaken
// the anchor. None grants s3:DeleteObject, s3:DeleteObjectVersion or s3:BypassGovernanceRetention.
//
// Replace the placeholders with PolicyFor. Grant s3:PutObjectLegalHold, and s3:GetObjectLegalHold
// to the reader, only if Config.LegalHold is set, and to a separate identity from the writer's if you want the
// hold to be releasable by someone other than the application.
var (
	//go:embed iam/writer.json
	WriterPolicy string
	//go:embed iam/reader.json
	ReaderPolicy string
	//go:embed iam/bucket.json
	BucketPolicy string
)

// PolicyFor fills a policy template in with the bucket name, the key prefix (as configured, with its
// trailing slash; "bide-anchor/" when empty) and the KMS key ARN.
func PolicyFor(template, bucket, prefix, kmsKeyARN string) string {
	if prefix == "" {
		prefix = "bide-anchor/"
	}
	return strings.NewReplacer("BUCKET", bucket, "PREFIX", prefix, "KMS_KEY_ARN", kmsKeyARN).Replace(template)
}
