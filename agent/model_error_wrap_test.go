package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// errModel fails every call with err.
type errModel struct{ err error }

func (m errModel) Stream(context.Context, Request) (*Stream, error) { return nil, m.err }

// A failed model call is an ErrModel, and its message says so once: an adapter's error that
// already wraps ErrModel (every provider adapter's does) is not wrapped in it again, which printed
// "(model) (model)".
func TestRun_ModelErrorSaysModelOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"adapter error wrapping ErrModel", fmt.Errorf("openai (%w)", ErrModel)},
		{"plain error", errors.New("connection refused")},
	} {
		_, err := mustNew(errModel{c.err}, memJournal()).Run(context.Background(), "r", UserText("hi"))
		if !errors.Is(err, ErrModel) {
			t.Errorf("%s: err = %v, want ErrModel", c.name, err)
			continue
		}
		if n := strings.Count(err.Error(), "(model)"); n != 1 {
			t.Errorf("%s: %q says (model) %d times, want once", c.name, err, n)
		}
	}
}
