package stopcmd

import (
	"context"

	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// JournalStores opens the SQLite and Postgres journals bide stop and bide-hsm stop use, with the event store
// the stop trail lives in: read-write to set, lift and write the trail, read-only to list and show.
func JournalStores() Stores {
	return Stores{
		SQLite: func(path string, readOnly bool) (Opened, error) {
			open := sqlite.Open
			if readOnly {
				open = sqlite.OpenReadOnly
			}
			s, err := open(path)
			if err != nil {
				return Opened{}, err
			}
			return Opened{Store: s, Events: s.EventStore(), Close: s}, nil
		},
		Postgres: func(dsn string, readOnly bool) (Opened, error) {
			ctx := context.Background()
			open := postgres.Open
			if readOnly {
				open = postgres.OpenReadOnly
			}
			s, err := open(ctx, dsn)
			if err != nil {
				return Opened{}, err
			}
			return Opened{Store: s, Events: s.EventStore(), Close: s}, nil
		},
	}
}
