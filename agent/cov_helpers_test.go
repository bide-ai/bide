package agent

import (
	"context"
	"errors"
	"strings"
)

// errCovBlip is the error covGetFail and covInsertFail inject.
var errCovBlip = errors.New("injected store fault")

// covGetFail fails every Get of an entry whose name contains sub.
type covGetFail struct {
	Store
	sub string
}

func (f covGetFail) Unwrap() Store { return f.Store }
func (f covGetFail) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if strings.Contains(name, f.sub) {
		return Entry{}, false, errCovBlip
	}
	return f.Store.Get(ctx, runID, name)
}

// covInsertFail fails, uncommitted, every Insert of an entry whose name contains sub.
type covInsertFail struct {
	Store
	sub string
}

func (f covInsertFail) Unwrap() Store { return f.Store }
func (f covInsertFail) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if strings.Contains(name, f.sub) {
		return Entry{}, false, errCovBlip
	}
	return f.Store.Insert(ctx, runID, name, data)
}
