// Package editcmd is bide edit: a local page that shows a flow config as the check reads it, in
// the flow editor (package flowedit). It serves on a loopback address only and reads (and, in later
// releases, writes) the one file named on the command line.
package editcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/bide-ai/bide/flowedit"
	"github.com/bide-ai/bide/internal/term"
)

const usageText = `usage: bide edit [-addr <host:port>] [-base <git revision>] [-color auto|always|never] <flow.yaml|flow.json>

Opens a local page for one flow config: the declared graph, an outline of its nodes and wiring,
each node's approval gate and safety, the assertions and their verdicts, the problems bide check
finds, and the source. The page shows what bide check and bide view -config show for the file,
computed by the same code, and labels the check as structural: block and type names are checked
when a program loads the flow with its plan.Registry.

You can edit the file's source in the page and see the check as you type; nothing is written until
you save, and a save shows the diff of the bytes it will write and the comments it removes. The page
compares the text with a baseline, the committed policy, and says so in a banner that stays while the file is weaker than
it: the file at git HEAD, or at the revision -base names (branch, tag or commit), or, when the file
is not in git, the file as it was when bide edit opened it. The comparison is the one bide check
-diff -check -fail-on-weaken makes.

It serves on a loopback address only (-addr, default 127.0.0.1:8585), answers only requests
addressed to a loopback host, and reads and writes only the file you name. The address it prints ends in a
fragment (#t=...) that carries a per-run session token; open that whole address. Stop it with
Ctrl-C. bide edit is not in a bide_audit_only build.

A save that weakens the policy against the baseline needs a confirmation that the server checks.
A save is refused, and writes nothing, if the file changed on disk since the page read it.
`

// Main runs bide edit with its arguments (after the command's name) and returns the exit status.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx, args, stdout, stderr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageText)
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:8585", "address to serve on: a loopback IP (127.0.0.1 or ::1) or localhost, with a port")
	base := fs.String("base", "", "compare with the file at this git revision (a branch, tag or commit) instead of at HEAD; the file must be in it")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	st, es := term.For(stdout, *color), term.For(stderr, *color)
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit: give exactly one flow config (a .yaml, .yml or .json file)")
		fs.Usage()
		return 2
	}
	var opts []flowedit.Option
	if *base != "" {
		opts = append(opts, flowedit.WithBase(*base))
	}
	srv, err := flowedit.New(fs.Arg(0), opts...)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", err)
		return 1
	}
	if n := srv.BaselineNote(); n != "" {
		fmt.Fprintln(stderr, es.Warn("bide edit: warning:")+" the baseline is "+n)
	}
	ln, err := flowedit.Listen(*addr)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", err)
		return 2
	}
	return serve(ctx, ln, srv, st, es, stdout, stderr)
}

// serve serves srv on ln until ctx is done. It prints the address to open, with the session token
// in its fragment.
func serve(ctx context.Context, ln net.Listener, srv *flowedit.Server, st, es term.Style, stdout, stderr io.Writer) int {
	link := "http://" + ln.Addr().String() + "/#t=" + srv.Token()
	fmt.Fprintf(stdout, "bide edit: serving %s %s\n", st.Path(link), st.Dim("(loopback only; Ctrl-C to stop)"))
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", err)
		return 1
	}
	return 0
}
