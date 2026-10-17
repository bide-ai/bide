package awss3_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/anchor/awss3"
)

type policy struct {
	Statement []struct {
		Effect string
		Action any
	}
}

func actions(t *testing.T, doc, effect string) []string {
	t.Helper()
	var p policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range p.Statement {
		if s.Effect != effect {
			continue
		}
		switch a := s.Action.(type) {
		case string:
			out = append(out, a)
		case []any:
			for _, x := range a {
				out = append(out, x.(string))
			}
		}
	}
	return out
}

func TestPoliciesGrantNoDeleteOrBypass(t *testing.T) {
	for name, doc := range map[string]string{"writer": awss3.WriterPolicy, "reader": awss3.ReaderPolicy} {
		for _, a := range actions(t, doc, "Allow") {
			l := strings.ToLower(a)
			if strings.Contains(l, "delete") || strings.Contains(l, "bypass") || strings.HasSuffix(l, ":*") || strings.Contains(l, "putbucket") || l == "s3:putlifecycleconfiguration" {
				t.Errorf("%s policy allows %s", name, a)
			}
		}
	}
	if slices.Contains(actions(t, awss3.ReaderPolicy, "Allow"), "s3:PutObject") {
		t.Error("the reader policy can write")
	}
	deny := actions(t, awss3.BucketPolicy, "Deny")
	for _, a := range []string{"s3:DeleteObjectVersion", "s3:BypassGovernanceRetention", "s3:PutBucketObjectLockConfiguration"} {
		if !slices.Contains(deny, a) {
			t.Errorf("the bucket policy does not deny %s", a)
		}
	}
}

// Every S3 operation the Anchor and the Reader make is allowed by the writer policy, so the policy is
// not narrower than the code needs. (The writer needs the reads: it reads each head back.)
func TestWriterPolicyCoversEveryCall(t *testing.T) {
	need := map[string][]string{
		"HeadBucket":                 {"s3:ListBucket"},
		"GetBucketVersioning":        {"s3:GetBucketVersioning"},
		"GetObjectLockConfiguration": {"s3:GetBucketObjectLockConfiguration"},
		"GetBucketEncryption":        {"s3:GetEncryptionConfiguration"},
		"PutObject":                  {"s3:PutObject", "s3:PutObjectRetention"},
		"GetObject":                  {"s3:GetObject", "s3:GetObjectVersion", "s3:GetObjectRetention", "s3:GetObjectLegalHold"},
		"ListObjectsV2":              {"s3:ListBucket"},
		"ListObjectVersions":         {"s3:ListBucketVersions"},
	}
	ty := reflect.TypeFor[awss3.Client]()
	allowed := actions(t, awss3.WriterPolicy, "Allow")
	for i := range ty.NumMethod() {
		m := ty.Method(i).Name
		acts, ok := need[m]
		if !ok {
			t.Errorf("Client method %s has no entry in this test: add the IAM actions it needs", m)
		}
		for _, a := range acts {
			if !slices.Contains(allowed, a) {
				t.Errorf("%s needs %s, which the writer policy does not allow", m, a)
			}
		}
	}
	for _, a := range []string{"kms:GenerateDataKey", "kms:Decrypt"} {
		if !slices.Contains(allowed, a) {
			t.Errorf("the writer policy lacks %s", a)
		}
	}
	filled := awss3.PolicyFor(awss3.WriterPolicy, "b", "", "arn:aws:kms:x")
	if strings.Contains(filled, "BUCKET") || strings.Contains(filled, "PREFIX") || !strings.Contains(filled, "arn:aws:s3:::b/bide-anchor/*") {
		t.Errorf("PolicyFor left placeholders or misplaced them: %s", filled)
	}
	if err := json.Unmarshal([]byte(filled), new(any)); err != nil {
		t.Errorf("filled policy is not JSON: %v", err)
	}
}
