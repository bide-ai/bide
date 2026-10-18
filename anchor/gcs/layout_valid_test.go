package gcs

import (
	"testing"

	"github.com/bide-ai/bide/audit/anchorobj"
)

// An object of one anchor must never be an object of the other: they differ in format and in partition
// tag. The S3 anchor's values are spelled out here because the gcs module must not import the AWS SDK;
// the bide-anchor module, which links both backends, repeats the check against the real values.
func TestLayoutIsValidAndDoesNotCollideWithS3(t *testing.T) {
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
	s3 := anchorobj.Layout{Format: "bide.audit.s3-anchor.v1", PartitionTag: "bide.audit.s3-anchor.partition.v1\x00"}
	if layout.Collides(s3) {
		t.Fatal("the gcs and awss3 layouts collide")
	}
	if layout.Partition("journal", "r") == s3.Partition("journal", "r") {
		t.Error("the same tree has the same partition under both anchors")
	}
}
