package sqlitelog_test

import (
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/eventlogtest"
	"github.com/bide-ai/bide/govern/sqlitelog"
)

// The SQLite log meets the EventLog contract, with each writer on its own handle to one file, as
// separate processes sharing the file would be.
func TestSQLiteLog_Conformance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	eventlogtest.Run(t, func(t *testing.T) govern.EventLog {
		l, err := sqlitelog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	})
}
