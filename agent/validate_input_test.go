package agent

import (
	"context"
	"errors"
	"testing"
)

func TestNew_RefusesNilModelOrJournal(t *testing.T) {
	if a, err := New(nil, memJournal()); !errors.Is(err, ErrConfig) || a != nil {
		t.Errorf("New(nil model) = %v, %v; want nil and ErrConfig", a, err)
	}
	if a, err := New(stubModel{}, nil); !errors.Is(err, ErrConfig) || a != nil {
		t.Errorf("New(nil journal) = %v, %v; want nil and ErrConfig", a, err)
	}
}

func TestEntryPoints_RejectEmptyRunID(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	a := mustNew(stubModel{}, s)

	if _, err := a.Run(ctx, "", UserText("hi")); !errors.Is(err, ErrConfig) {
		t.Errorf("Run(empty) err = %v, want ErrConfig", err)
	}
	if _, err := a.Run(ctx, "", UserText("hi"), WithSaga()); !errors.Is(err, ErrConfig) {
		t.Errorf("saga Run(empty) err = %v, want ErrConfig", err)
	}
	if _, err := a.Session(ctx, ""); !errors.Is(err, ErrConfig) {
		t.Errorf("Session(empty) err = %v, want ErrConfig", err)
	}
	if err := Approve(ctx, s, "", "tu", true); !errors.Is(err, ErrConfig) {
		t.Errorf("Approve(empty) err = %v, want ErrConfig", err)
	}
	if err := s.AnswerInterrupt(ctx, "", "k", 1); !errors.Is(err, ErrConfig) {
		t.Errorf("Resume(empty) err = %v, want ErrConfig", err)
	}
}

// stubModel is a minimal Model that never gets called (the validation guards fire first).
type stubModel struct{}

func (stubModel) Stream(context.Context, Request) (*Stream, error) { return nil, nil }
