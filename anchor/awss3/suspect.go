package awss3

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	// suspectDigits is the length of a contiguous digit run that suspectID reports (an account number
	// has 8 to 12 digits; a shorter bound would flag every date-and-sequence run ID).
	suspectDigits = 12
	// suspectSeparatedDigits is the length of a run of digits that suspectID reports when spaces or
	// hyphens separate them (a card number has 13 to 19 digits, written in groups).
	suspectSeparatedDigits = 13
)

// notifyOpenPrefix is the start of the notification trail names the notifier writes
// ("notify:@open:<date>", "notify:@open:index:<year>"): its '@' is part of the name, not an address.
const notifyOpenPrefix = "notify:@open:"

// suspectID returns a reason when a run or trail ID looks like it holds personal data, or "". The
// anchor is write-once: an object cannot be erased before its retention ends, and a signed head
// carries its run ID in clear (the ID is part of the signed bytes). The check is a tripwire for the
// obvious cases and not a proof that an ID is free of personal data. It reports:
//   - an '@' with something before it and a dotted host (or localhost) after it, an email address, in
//     any ID but for the exact notification trail prefix notifyOpenPrefix, which is not examined;
//   - a run of suspectDigits or more digits, or of suspectSeparatedDigits or more digits that spaces
//     and hyphens separate (an account or card number).
//
// It does not report a name, an address, a free-text description, an email address written with
// spaces or an obfuscation, a number in another notation, or anything that merely looks opaque.
func suspectID(id string) string {
	id = strings.TrimPrefix(id, notifyOpenPrefix)
	if looksLikeEmail(id) {
		return "looks like an email address"
	}
	contiguous, separated := 0, 0
	for _, r := range id {
		switch {
		case unicode.IsDigit(r):
			contiguous++
			separated++
			if contiguous >= suspectDigits || separated >= suspectSeparatedDigits {
				return fmt.Sprintf("holds a run of %d or more digits (an account or card number?)", suspectDigits)
			}
		case r == '-' || r == ' ':
			contiguous = 0 // separators join digits for the card rule only
		default:
			contiguous, separated = 0, 0
		}
	}
	return ""
}

// looksLikeEmail reports an '@' that has a character before it and a host after it with a dot
// (user@example.com) or that is localhost.
func looksLikeEmail(id string) bool {
	for i := 0; i < len(id); i++ {
		if id[i] != '@' || i == 0 {
			continue
		}
		host := id[i+1:]
		if end := strings.IndexAny(host, " /\\,;:\"'<>()[]"); end >= 0 {
			host = host[:end]
		}
		if strings.EqualFold(host, "localhost") {
			return true
		}
		if dot := strings.IndexByte(host, '.'); dot > 0 && dot < len(host)-1 {
			return true
		}
	}
	return false
}
