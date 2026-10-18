// Package auditstores opens the SQLite and Postgres stores bide audit reads with -db and -postgres-env,
// read-only. The bide command and bide-hsm audit stops share it; the audit-only build links no driver and
// does not import it.
package auditstores

import (
	"context"

	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// Openers returns the store openers for auditcli.Stores.
func Openers() *auditcli.StoreOpeners {
	return &auditcli.StoreOpeners{
		SQLite: func(path string) (auditcli.NotifyStores, error) {
			s, err := sqlite.OpenReadOnly(path)
			if err != nil {
				return auditcli.NotifyStores{}, err
			}
			return auditcli.NotifyStores{Journal: s, Events: s.EventStore(), Close: s.Close}, nil
		},
		Postgres: func(dsn string) (auditcli.NotifyStores, error) {
			s, err := postgres.OpenReadOnly(context.Background(), dsn)
			if err != nil {
				return auditcli.NotifyStores{}, err
			}
			return auditcli.NotifyStores{Journal: s, Events: s.EventStore(), Close: s.Close}, nil
		},
	}
}
