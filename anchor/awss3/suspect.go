package awss3

import (
	"fmt"
	"strings"
	"unicode"
)

// suspectDigits is the length of a digit run that suspectID reports (a card number has 13 or more,
// an account number 8 to 12; a shorter bound would flag every date-and-sequence run ID).
const suspectDigits = 12

// suspectID returns a reason when a run or trail ID looks like it holds personal data, or "". The
// anchor is write-once: an object cannot be erased before its retention ends, and a signed head
// carries its run ID in clear (the ID is part of the signed bytes). The check is a tripwire for the
// obvious cases, an email address or a long run of digits (an account or card number), and is not a
// proof that an ID is free of personal data.
func suspectID(id string) string {
	if at := strings.IndexByte(id, '@'); at > 0 {
		// notify trail IDs are "notify:@open:<date>": an '@' right after ':' is not an address.
		if i := strings.IndexByte(id[at+1:], '.'); i > 0 && !strings.HasPrefix(id[at:], "@open") && id[at-1] != ':' {
			return "looks like an email address"
		}
	}
	run := 0
	for _, r := range id {
		if unicode.IsDigit(r) {
			run++
			if run >= suspectDigits {
				return fmt.Sprintf("holds a run of %d or more digits (an account or card number?)", suspectDigits)
			}
			continue
		}
		run = 0
	}
	return ""
}
