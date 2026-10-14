package agent

import (
	"context"
	"errors"
	"testing"
)

// WithOutputMode on an untyped run is ErrConfig at its first drive, before anything is journaled
// or any model call is made, as it is at a later drive.
func TestWithOutputModeOnUntypedFirstDriveIsErrConfig(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	m := &scriptModel{turns: [][]Emit{errTurn(errors.New("down")), textTurn("ok")}}
	a := mustNew(m, j)
	if _, err := a.Run(ctx, "r", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) {
		t.Fatalf("first drive = %v; want ErrConfig", err)
	}
	if _, ok, err := j.Get(ctx, "r", runStartStep); err != nil || ok {
		t.Fatalf("the refused drive journaled run:start (%v)", err)
	}
	if m.i != 0 {
		t.Fatalf("the refused drive made %d model calls", m.i)
	}
	if _, err := a.Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("setup: the first model turn should fail, leaving the run open")
	}
	if _, err := a.Run(ctx, "r", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) {
		t.Fatalf("later drive = %v; want ErrConfig", err)
	}
}
