package approvecmd

import (
	"context"
	"os"
	"os/signal"
)

// signalContext is cancelled on an interrupt, so Ctrl-C at the confirmation prompt or during a
// slow store call stops the command without recording anything half-done.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
