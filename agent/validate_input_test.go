package agent

import (
	"context"
	"errors"
	"testing"
)

func TestNew_PanicsOnNilModelOrStore(t *testing.T) {
	mustPanicNew := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic, got none", name)
			}
		}()
		fn()
	}
	mustPanicNew("nil model", func() { New(nil, memJournal()) })
	mustPanicNew("nil store", func() { mustNew(stubModel{}, nil) })
}

func TestEntryPoints_RejectEmptyRunID(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	a := mustNew(stubModel{}, s)

	if _, err := a.Run(ctx, "", UserText("hi")); !errors.Is(err, ErrConfig) {
		t.Errorf("Run(empty) err = %v, want ErrConfig", err)
	}
	if _, err := a.Run(ctx, "", UserText("hi"), WithSaga()); !errors.Is(err, ErrConfig) {
		t.Errorf("RunSaga(empty) err = %v, want ErrConfig", err)
	}
	if _, err := a.Session(ctx, ""); !errors.Is(err, ErrConfig) {
		t.Errorf("Session(empty) err = %v, want ErrConfig", err)
	}
	if err := Approve(ctx, s, "", "tu", true); !errors.Is(err, ErrConfig) {
		t.Errorf("Approve(empty) err = %v, want ErrConfig", err)
	}
	if err := Resume(ctx, s, "", "k", 1); !errors.Is(err, ErrConfig) {
		t.Errorf("Resume(empty) err = %v, want ErrConfig", err)
	}
}

// stubModel is a minimal Model that never gets called (the validation guards fire first).
type stubModel struct{}

func (stubModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
