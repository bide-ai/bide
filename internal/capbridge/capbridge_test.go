package capbridge

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/internal/capledger"
)

func TestRegisterVoided_OnceOnlyAndNeverNil(t *testing.T) {
	saved := voided
	voided = nil
	t.Cleanup(func() { voided = saved })

	if Voided() != nil {
		t.Fatal("Voided is set before any registration")
	}
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("a nil function", func() { RegisterVoided(nil) })
	first := func(context.Context, any, capledger.Slot) (bool, error) { return true, nil }
	RegisterVoided(first)
	if ok, _ := Voided()(context.Background(), nil, capledger.Slot{}); !ok {
		t.Fatal("Voided is not the registered function")
	}
	mustPanic("a second registration", func() {
		RegisterVoided(func(context.Context, any, capledger.Slot) (bool, error) { return false, nil })
	})
	if ok, _ := Voided()(context.Background(), nil, capledger.Slot{}); !ok {
		t.Fatal("a refused second registration replaced the first")
	}
}
