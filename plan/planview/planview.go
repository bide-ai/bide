// Package planview embeds bide view's flow-config page in a program, with the program's
// plan.Registry, so the page checks a config the way plan.Load does: blocks, predicates, merges and
// types, as well as structure, and shows the digest a run of the flow would journal. The bide view
// command cannot import a program's Go types, so its -config page is structural; this package is
// the full check. Nothing runs, and the page is read-only.
package planview

import (
	"io"
	"net"
	"net/http"

	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// Handler serves the config page for the flow config data (JSON), named name in messages, checked
// against reg (nil: structural only, as the bide view command checks it): the page at / and its
// view as JSON at /config.json. It answers only GET and HEAD requests addressed to a loopback host.
func Handler(name string, data []byte, reg *plan.Registry) http.Handler {
	return runview.Handler(runview.Options{Config: &runview.Config{Name: name, Data: data, Registry: reg}})
}

// Listen listens on addr, which must name a loopback IP or localhost (any other host is refused), for
// serving Handler: http.Serve(ln, planview.Handler(...)).
func Listen(addr string) (net.Listener, error) { return runview.Listen(addr) }

// Check writes the config's problems to w as "name:node: message" lines ("name:from->to: message"
// for an edge or arm, "name: message" for the config as a whole), warnings marked "warning:", and
// reports whether any is an error: the bide view -check output, with reg's checks added. See
// plan.InspectConfig for the problems themselves.
func Check(w io.Writer, name string, data []byte, reg *plan.Registry) bool {
	return runview.Check(w, runview.Config{Name: name, Data: data, Registry: reg})
}
