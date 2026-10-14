package approvecmd

import (
	"context"
	"io"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// JournalStores opens the SQLite and Postgres journals bide approve and bide-hsm decide on:
// read-write to record a decision, read-only to list the calls that wait.
func JournalStores() Stores {
	return Stores{
		SQLite: func(path string, readOnly bool) (agent.Store, io.Closer, error) {
			if readOnly {
				s, err := sqlite.OpenReadOnly(path)
				if err != nil {
					return nil, nil, err
				}
				return s, s, nil
			}
			s, err := sqlite.Open(path)
			if err != nil {
				return nil, nil, err
			}
			return s, s, nil
		},
		Postgres: func(dsn string, readOnly bool) (agent.Store, io.Closer, error) {
			if readOnly {
				s, err := postgres.OpenReadOnly(context.Background(), dsn)
				if err != nil {
					return nil, nil, err
				}
				return s, s, nil
			}
			s, err := postgres.Open(context.Background(), dsn)
			if err != nil {
				return nil, nil, err
			}
			return s, s, nil
		},
	}
}
