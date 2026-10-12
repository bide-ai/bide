package runview

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoStoreAdapters: the viewer is in the core module, which carries no store driver. The
// bide-view command (its own module, cmd/bide-view) plugs the stores in through Stores.
func TestNoStoreAdapters(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/bide-ai/bide/internal/runview").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, bad := range []string{"bide-ai/bide/store/", "modernc.org/sqlite", "jackc/pgx"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("internal/runview depends on %q; the core module must stay free of store adapters", bad)
		}
	}
}
