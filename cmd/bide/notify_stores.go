//go:build !bide_audit_only

package main

import (
	"context"

	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// bide audit notifications -db and -postgres-env read the journal and the notification trails from
// a store, read-only, as bide view does. The audit-only build links no driver, so it leaves
// auditcli.Stores nil and the verb reads exported files only.
func init() {
	auditcli.Stores = &auditcli.StoreOpeners{
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
