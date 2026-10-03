package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/schema"
)

type hiddenArgs struct {
	Q string `json:"q"`
}

// undecodableArgs reaches its only field through an embedded pointer to an unexported struct,
// which encoding/json cannot allocate: no call's arguments could ever decode.
type undecodableArgs struct {
	*hiddenArgs
}

// Func refuses an argument type no call could decode, at construction, rather than build a tool
// with no schema whose every call fails.
func TestFunc_UndescribableArgsPanics(t *testing.T) {
	defer func() {
		r := recover()
		err, _ := r.(error)
		if !errors.Is(err, schema.ErrUnsupportedType) || !strings.Contains(fmt.Sprint(r), `"q"`) {
			t.Fatalf("Func panicked with %v; want an error wrapping schema.ErrUnsupportedType naming the field", r)
		}
	}()
	MustFunc("t", "", func(context.Context, undecodableArgs) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}))
}

// RunTyped reports such a result type as a configuration error before running anything.
func TestRunTyped_UndescribableTypeIsConfigError(t *testing.T) {
	a := mustNew(NewScriptedModel(TextTurn(`{}`)), memJournal())
	if _, _, err := a.RunTyped[undecodableArgs](context.Background(), "r", UserText("go")); !errors.Is(err, ErrConfig) || !errors.Is(err, schema.ErrUnsupportedType) {
		t.Fatalf("RunTyped = %v; want ErrConfig wrapping schema.ErrUnsupportedType", err)
	}
	if _, _, err := a.RunTyped[undecodableArgs](context.Background(), "r2", UserText("go"), WithOutputMode(OutputNative)); !errors.Is(err, ErrConfig) || !errors.Is(err, schema.ErrUnsupportedType) {
		t.Fatalf("RunTyped (OutputNative) = %v; want ErrConfig wrapping schema.ErrUnsupportedType", err)
	}
}
