package awss3

import (
	"fmt"
	"regexp"
)

// A KMS key is named by exactly one of: its key ID (a lower-case UUID), a multi-Region key ID ("mrk-" and 32
// lower-case hex digits), or its key ARN (arn:<partition>:kms:<region>:<account>:key/<one of those>). Anything
// else, an alias above all, is refused where a key is configured: S3 reports the ARN of the key an alias
// resolves to, so a check against an alias could not tell which key it was and would take any key for the
// one meant, and a string that is no key at all can never equal what S3 reports.
var (
	keyIDPattern = `(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})`
	keyIDRe      = regexp.MustCompile(`^` + keyIDPattern + `$`)
	keyARNRe     = regexp.MustCompile(`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/` + keyIDPattern + `$`)
)

// validKMSKey reports whether k is a key ID, a multi-Region key ID or a key ARN.
func validKMSKey(k string) bool { return keyIDRe.MatchString(k) || keyARNRe.MatchString(k) }

// checkKMSKey returns nil for a valid key reference and an error that says what to pass otherwise.
func checkKMSKey(field, k string) error {
	if validKMSKey(k) {
		return nil
	}
	return fmt.Errorf("%s %q is not a KMS key ID (a UUID), a multi-Region key ID (mrk- and 32 hex digits) or a key ARN (arn:<partition>:kms:<region>:<account>:key/<id>); an alias is refused, since S3 reports the ARN of the key an alias resolves to and the check could not tell which key it was: pass the ARN of the key the alias points to", field, k)
}
