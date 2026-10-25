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
	"strconv"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/flowedit"
	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/term"
)

const usageText = `usage: bide edit [-addr <host:port>] [-base <git revision>] [-blocks <description.json>] [-run <id>] [-db <file> | -postgres <dsn> | -postgres-env <VAR> | -journal <export.json>] [-theme <theme.json>] [-theme-default <name>] [-color auto|always|never] <flow.yaml|flow.json>

Opens a local page for one flow config: the declared graph, an outline of its nodes and wiring,
each node's approval gate and safety, the assertions and their verdicts, the problems bide check
finds, and the source. The page shows what bide check and bide view -config show for the file,
computed by the same code, and labels the check as structural: block and type names are checked
when a program loads the flow with its plan.Registry.

With -blocks, a description of the program's registered blocks (json.Marshal of plan.Registry.Describe),
the editor is typed: it lists the blocks in a palette with their types, safety and gates, checks the
types along every connection, shows the digest a run would journal, refuses an operation that
connects what does not fit, and knows the gate a block declares in Go when a block is changed. Every
typed result is labelled as resting on the file, not checked against the program: the check made
with the program's own plan.Registry is the control. A program can embed the editor beside its
Registry instead (package flowedit, WithRegistry).

With -run and a store (the same flags bide view takes, opened read-only), the page also compares the
file with the topology that run journaled: it says whether the file has the digest the run journaled
and, when it differs, what differs, by what the digest covers (flow name, entry, nodes, blocks, edges,
switch arms, gates when the blocks are known, and their order). That is a comparison of topologies and
nothing more: it does not say the run followed the file, which is conformance (bide view). A run that
is paused under a topology the file no longer has will not resume under the file. The page reads the
run once and again only when you press Reload run. With a store, with or without -run, the page's button,
Check unfinished runs, lists the store's unfinished runs by the digest each recorded and says which a drive
under this file (resume, recovery or restart) would refuse by the digest rule: a run whose digest is the
file's is not refused, a run of another flow is counted apart, and a run that has recorded no digest yet is
counted and not read. It reads one small record per run (at most 5000 runs, the first by run ID) and never a
history.

The page draws from checked themes: light, dark and high-contrast, which its header button steps
through, and the system's request for more contrast or forced colors is answered as bide view answers
it. -theme adds a theme file (see docs/guides/themes.md and bide theme check), which is refused, with
the failed checks, and nothing is served, if it is not valid or fails a check; -theme-default starts
the page in a theme. A program that embeds the editor can also lock it to one theme (package
flowedit, WithThemeLock).

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
// stores opens the journal -run reads, read-only.
func Main(args []string, stdout, stderr io.Writer, stores runview.Stores) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx, args, stdout, stderr, stores)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, stores runview.Stores) int {
	fs := flag.NewFlagSet("bide edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageText)
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:8585", "address to serve on: a loopback IP (127.0.0.1 or ::1) or localhost, with a port")
	base := fs.String("base", "", "compare with the file at this git revision (a branch, tag or commit) instead of at HEAD; the file must be in it")
	blocks := fs.String("blocks", "", "a description of the program's registered blocks (json.Marshal of plan.Registry.Describe): typed mode, labelled as per the file, not checked against the program")
	var sf runview.StoreFlags
	sf.Register(fs)
	runID := fs.String("run", "", "a run of the journal to compare the file with: the topology it journaled, and what differs from the file (needs one of -db, -postgres, -postgres-env, -journal; a store without -run is for the list of unfinished runs)")
	themeFile := fs.String("theme", "", runview.ThemeFlagHelp)
	themeDefault := fs.String("theme-default", "", runview.ThemeDefaultFlagHelp)
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
	usage := func(msg string) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", msg)
		fs.Usage()
		return 2
	}
	if msg := sf.Validate(); msg != "" {
		return usage(msg)
	}
	switch {
	case sf.Kinds() == 0 && *runID != "":
		return usage("-run needs a store: one of -db, -postgres, -postgres-env or -journal")
	case agent.IsReservedRun(*runID):
		return usage("-run: " + strconv.Quote(*runID) + " is not a run: the ID is reserved by the engine")
	}
	// The theme file is read after the other flags have been validated (a flag error is a usage error
	// whatever the file holds) and before anything is opened or served.
	themes, usageMsg, code := runview.ThemeFlags(*themeFile, *themeDefault, stderr, es.ErrorPrefix())
	if usageMsg != "" {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", usageMsg)
		fs.Usage()
		return 2
	}
	if code != 0 {
		return code
	}
	var opts []flowedit.Option
	opts = append(opts, flowedit.WithThemes(themes.Extra...), flowedit.WithDefaultTheme(themes.Default))
	if *base != "" {
		opts = append(opts, flowedit.WithBase(*base))
	}
	if *blocks != "" {
		d, err := runview.ReadDescription(*blocks)
		if err != nil {
			fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", err)
			return 1
		}
		opts = append(opts, flowedit.WithBlocks(*d))
	}
	if sf.Kinds() == 1 {
		op, err := sf.Open(stores)
		if err != nil {
			fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", err)
			return 1
		}
		if op.Closer != nil {
			defer op.Closer.Close()
		}
		if editrun.NewOption == nil {
			fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit: this build cannot compare with a run")
			return 1
		}
		opts = append(opts, editrun.NewOption(editrun.Run{ID: *runID, Source: op.Source}).(flowedit.Option))
	}
	srv, err := flowedit.New(fs.Arg(0), opts...)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide edit:", err)
		return 1
	}
	defer srv.Close()
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
