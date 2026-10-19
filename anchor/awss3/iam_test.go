package awss3_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	filled := awss3.PolicyFor(awss3.WriterPolicy, awss3.PolicyParams{Bucket: "b", KMSKeyARN: "arn:aws:kms:x"})
	if strings.Contains(filled, "BUCKET") || strings.Contains(filled, "PREFIX") || !strings.Contains(filled, "arn:aws:s3:::b/bide-anchor/*") {
		t.Errorf("PolicyFor left placeholders or misplaced them: %s", filled)
	}
	if err := json.Unmarshal([]byte(filled), new(any)); err != nil {
		t.Errorf("filled policy is not JSON: %v", err)
	}
}

// The filled bucket policy denies a writer a weaker lock, a shorter retention, another KMS key, and
// changing the policy itself; and every placeholder is filled.
func TestBucketPolicyPinsLockRetentionAndKey(t *testing.T) {
	filled := awss3.PolicyFor(awss3.BucketPolicy, awss3.PolicyParams{Bucket: "b", KMSKeyARN: "arn:aws:kms:eu-west-1:111122223333:key/k",
		MinRetentionDays: 2555, AdminRoleARN: "arn:aws:iam::444455556666:role/anchor-admin"})
	for _, ph := range []string{"BUCKET", "PREFIX", "KMS_KEY_ARN", "LOCK_MODE", "MIN_RETENTION_DAYS", "ADMIN_ROLE_ARN"} {
		if strings.Contains(filled, ph) {
			t.Errorf("the filled bucket policy still holds %s", ph)
		}
	}
	var p struct {
		Statement []struct {
			Effect, Sid string
			Condition   map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(filled), &p); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{ // sid: operator, key, value
		"OnlyLockedHeads":                       {"StringNotEquals", "s3:object-lock-mode", "COMPLIANCE"},
		"OnlyLongRetention":                     {"NumericLessThan", "s3:object-lock-remaining-retention-days", "2555"},
		"OnlyTheConfiguredKmsKey":               {"StringNotEquals", "s3:x-amz-server-side-encryption-aws-kms-key-id", "arn:aws:kms:eu-west-1:111122223333:key/k"},
		"OnlyTheAdministratorChangesThisPolicy": {"StringNotEquals", "aws:PrincipalArn", "arn:aws:iam::444455556666:role/anchor-admin"},
	}
	for _, st := range p.Statement {
		w, ok := want[st.Sid]
		if !ok {
			continue
		}
		delete(want, st.Sid)
		if st.Effect != "Deny" || st.Condition[w[0]][w[1]] != w[2] {
			t.Errorf("%s = %v, want a Deny with %s %s = %s", st.Sid, st.Condition, w[0], w[1], w[2])
		}
	}
	for sid := range want {
		t.Errorf("the bucket policy has no %s statement", sid)
	}
	if g := awss3.PolicyFor(awss3.BucketPolicy, awss3.PolicyParams{Bucket: "b", LockMode: "GOVERNANCE"}); !strings.Contains(g, `"s3:object-lock-mode": "GOVERNANCE"`) {
		t.Error("LockMode did not reach the policy")
	}
}

// Every S3 call the Reader makes is allowed by the reader policy, and the policy has no write.
func TestReaderPolicyCoversEveryReaderCall(t *testing.T) {
	allowed := actions(t, awss3.ReaderPolicy, "Allow")
	for call, acts := range map[string][]string{
		"ListObjectVersions":         {"s3:ListBucketVersions"},
		"GetObject":                  {"s3:GetObject", "s3:GetObjectVersion", "s3:GetObjectRetention", "s3:GetObjectLegalHold"},
		"GetBucketVersioning":        {"s3:GetBucketVersioning"},
		"GetObjectLockConfiguration": {"s3:GetBucketObjectLockConfiguration"},
		"GetBucketEncryption":        {"s3:GetEncryptionConfiguration"},
	} {
		for _, a := range acts {
			if !slices.Contains(allowed, a) {
				t.Errorf("the Reader calls %s, which needs %s, but the reader policy does not allow it", call, a)
			}
		}
	}
	for _, a := range allowed {
		if strings.HasPrefix(a, "s3:Put") || strings.Contains(strings.ToLower(a), "delete") {
			t.Errorf("the reader policy allows %s", a)
		}
	}
}

// The guide names every action the shipped reader policy grants for reading the bucket's configuration, and
// the changelog fragment that tells readers to add them names the same three.
func TestGuideAndChangelogNameTheReaderActions(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", "anchoring.md"))
	if err != nil {
		t.Fatal(err)
	}
	frag, err := os.ReadFile(filepath.Join("..", "..", "changes", "unreleased", "awss3-bucket-configuration.md"))
	if err != nil {
		t.Skip("the changelog fragment is assembled at release: " + err.Error())
	}
	policy := actions(t, awss3.ReaderPolicy, "Allow")
	for _, a := range []string{"s3:GetBucketVersioning", "s3:GetBucketObjectLockConfiguration", "s3:GetEncryptionConfiguration"} {
		if !slices.Contains(policy, a) {
			t.Errorf("the reader policy lacks %s", a)
		}
		if !strings.Contains(string(guide), a) {
			t.Errorf("the guide does not name %s", a)
		}
		if !strings.Contains(string(frag), a) {
			t.Errorf("the changelog fragment does not name %s", a)
		}
	}
}
