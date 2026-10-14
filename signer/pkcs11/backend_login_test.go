//go:build cgo

package pkcs11

import (
	"errors"
	"testing"

	p11 "github.com/miekg/pkcs11"

	"github.com/bide-ai/bide/agent"
)

// fakeLogin records the logins a token receives.
type fakeLogin struct {
	pins []string
	err  error
}

func (f *fakeLogin) Login(_ p11.SessionHandle, user uint, pin string) error {
	f.pins = append(f.pins, pin)
	return f.err
}

// A token with a protected authentication path (a PIN pad) is logged in with no PIN, when asked;
// a token without one is refused that, and a PIN given to a PIN pad is refused too.
func TestLoginProtectedAuthPath(t *testing.T) {
	const pad, none = p11.CKF_PROTECTED_AUTHENTICATION_PATH, 0
	pinSrc := func() (string, error) { return "246810", nil }
	for _, tc := range []struct {
		name  string
		flags uint
		cfg   Config
		want  []string // PINs passed to C_Login; nil means no login
		err   error
	}{
		{"pad, protected", pad, Config{ProtectedAuthPath: true}, []string{""}, nil},
		{"no pad, protected", none, Config{ProtectedAuthPath: true}, nil, ErrNoProtectedAuthPath},
		{"pad, PIN given", pad, Config{PIN: pinSrc}, nil, agent.ErrConfig},
		{"no pad, PIN given", none, Config{PIN: pinSrc}, []string{"246810"}, nil},
		{"no pad, no login", none, Config{}, nil, nil},
		{"pad, no login", pad, Config{}, nil, nil},
	} {
		f := &fakeLogin{}
		pin := ""
		if tc.cfg.PIN != nil {
			pin = "246810"
		}
		err := login(f, 1, tc.flags, tc.cfg, pin)
		if (tc.err == nil) != (err == nil) || (tc.err != nil && !errors.Is(err, tc.err)) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.err)
		}
		if len(f.pins) != len(tc.want) || (len(tc.want) == 1 && f.pins[0] != tc.want[0]) {
			t.Errorf("%s: logins = %q, want %q", tc.name, f.pins, tc.want)
		}
		if err != nil && errors.Is(err, ErrNoProtectedAuthPath) && !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: ErrNoProtectedAuthPath is a configuration error", tc.name)
		}
	}
	// Whatever PIN the caller holds is never sent to a PIN pad.
	f2 := &fakeLogin{}
	if err := login(f2, 1, pad, Config{ProtectedAuthPath: true}, "246810"); err != nil || len(f2.pins) != 1 || f2.pins[0] != "" {
		t.Errorf("a PIN reached the pad login: %v %q", err, f2.pins)
	}
	// A pad PIN that the token rejects is the token's error, and an already logged-in user is fine.
	f := &fakeLogin{err: errors.New("CKR_PIN_INCORRECT")}
	if err := login(f, 1, pad, Config{ProtectedAuthPath: true}, ""); err == nil {
		t.Error("a rejected login succeeded")
	}
}
