package stopflood

import (
	"testing"
	"time"
)

// The growth check passes cost linear in the entries and fails a quadratic one, so the guard is itself tested.
func TestLinear(t *testing.T) {
	small := 100 * time.Millisecond
	for _, c := range []struct {
		name  string
		large time.Duration
		want  bool
	}{
		{"linear", 4 * small, true},
		{"a little worse than linear", 6 * small, true},
		{"quadratic", 16 * small, false},
		{"noise near zero", 40 * time.Millisecond, true},
	} {
		if got := Linear(small, c.large); got != c.want {
			t.Errorf("%s: Linear(%v, %v) = %v", c.name, small, c.large, got)
		}
	}
	if Linear(time.Millisecond, 200*time.Millisecond) {
		t.Error("a 200-fold growth from a millisecond passed")
	}
}
