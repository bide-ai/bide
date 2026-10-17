package journalcmd

import (
	"context"
	"os"
	"os/signal"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
