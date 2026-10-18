//go:build !bide_audit_only

package main

import (
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/cmd/bide/internal/auditstores"
	"github.com/bide-ai/bide/internal/auditcli"
)

// bide audit notifications -db and -postgres-env read the journal and the notification trails from
// a store, read-only, as bide view does. The audit-only build links no driver, so it leaves
// auditcli.Stores nil and the verb reads exported files only.
func init() {
	// bide audit stops -sign-head signs with a key file, as bide stop does.
	auditcli.LoadLogSigner = approvecmd.LoadSigner
	auditcli.Stores = auditstores.Openers()
}
