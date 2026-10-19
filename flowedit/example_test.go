package flowedit_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bide-ai/bide/flowedit"
	"github.com/bide-ai/bide/plan"
)

type Order struct{ ID string }
type Receipt struct{ ID string }

// A program that embeds the editor beside its Registry: the editor is typed against the program's
// own blocks (it checks the config as Load does, lists the blocks, shows the digest and refuses a
// connection that does not fit), and the host decides only what is below: the file, the Registry and
// where the handler is mounted. The listener, the host and origin checks, the token and the one file
// it may write are the handler's, whatever it is mounted on.
func ExampleWithRegistry() {
	reg := plan.NewRegistry()
	if err := reg.RegisterStep("validate", func(_ context.Context, o Order) (Order, error) { return o, nil }, plan.ReadOnly()); err != nil {
		log.Fatal(err)
	}
	if err := reg.RegisterStep("receipt", func(_ context.Context, o Order) (Receipt, error) { return Receipt{o.ID}, nil }); err != nil {
		log.Fatal(err)
	}
	if err := reg.RegisterStep("refund", func(_ context.Context, o Order) (Receipt, error) { return Receipt{o.ID}, nil }); err != nil {
		log.Fatal(err)
	}

	dir, err := os.MkdirTemp("", "editor")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "flow.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nflow: orders\nentry: validate\nnodes:\n  - {name: validate, block: validate}\n  - {name: receipt, block: receipt}\nwiring:\n  - edge: [validate, receipt]\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	srv, err := flowedit.New(path, flowedit.WithRegistry(reg))
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	// Serve it: Listen refuses anything but a loopback address, and the handler answers only loopback
	// hosts with the session token (srv.Token(), carried in the URL fragment the host prints).
	var _ http.Handler = srv.Handler()

	doc, err := srv.Doc()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("typed against:", doc.Typed.Source)
	fmt.Println("problems:", doc.Errors)
	for _, b := range doc.Typed.Palette {
		fmt.Printf("%s used=%v\n", b.Name, b.Used)
	}
	// Output:
	// typed against: registry
	// problems: 0
	// receipt used=true
	// refund used=false
	// validate used=true
}
