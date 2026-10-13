package agent

import (
	"fmt"
	"time"
)

// timestampLayout is the one form of a time bide records: RFC 3339 in UTC, to the millisecond,
// with a literal "Z".
const timestampLayout = "2006-01-02T15:04:05.000Z"

// FormatTimestamp returns t in the one form bide records times in (RunStart.StartedAt, a signed
// decision's decided_at, the recorded_at of a decision and of ApprovalWait): RFC 3339 in UTC,
// truncated to the millisecond, always 24 bytes and ending in "Z", e.g.
// "2026-10-13T12:14:03.123Z". A time outside the years 0000 to 9999 has no such form: what
// FormatTimestamp returns for it, ParseTimestamp refuses.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format(timestampLayout)
}

// ParseTimestamp reads s, which must be in exactly the form FormatTimestamp writes: it parses s
// and refuses it unless formatting the result again gives s byte for byte. So it refuses another
// offset ("+00:00", "+02:00"), more or fewer fractional digits than three, a missing fraction, a
// lowercase "t" or "z", a leap second ("23:59:60.000Z"), and any date or time that does not exist.
// A signed decision time is checked with it, so one instant has one signed spelling.
func ParseTimestamp(s string) (time.Time, error) {
	if len(s) != len(timestampLayout) {
		return time.Time{}, fmt.Errorf("timestamp %q is not of the form 2006-01-02T15:04:05.000Z (24 bytes)", s)
	}
	t, err := time.Parse(timestampLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not of the form 2006-01-02T15:04:05.000Z: %w", s, err)
	}
	if FormatTimestamp(t) != s {
		return time.Time{}, fmt.Errorf("timestamp %q is not in canonical form (want %q)", s, FormatTimestamp(t))
	}
	return t, nil
}

// noteStamp is an informational recorded_at for the time now returns (an approval request's or a
// pause's): FormatTimestamp's form, or "" when the time has none, since such a note never fails
// the drive that writes it and a reader treats "" as not recorded.
func noteStamp(now func() time.Time) string {
	s := FormatTimestamp(now())
	if _, err := ParseTimestamp(s); err != nil {
		return ""
	}
	return s
}
