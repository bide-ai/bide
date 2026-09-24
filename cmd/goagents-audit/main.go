// Command goagents-audit produces and verifies audit ProofBundles from the command line, for
// the auditor/compliance persona who does not write Go. It is deliberately dependency-light:
// it imports only the core + audit packages (stdlib under the hood), no store backend, so it
// works against any store by operating on an exported journal (a JSON array of records) and a
// signed tree head. `verify` needs neither: just the bundle and a public key.
//
//	# Produce a proof that one tool call happened, against an anchored STH:
//	goagents-audit prove -journal run.json -sth sth.json -tool call_abc -out proof.json
//
//	# Verify it offline, trusting only an out-of-band public key:
//	goagents-audit verify -bundle proof.json -pubkey 1a2b...   # exit 0 = verified
//
// Export the journal with: json.Marshal(store.History(ctx, runID)).
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
)

func flagSet(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ExitOnError) }

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "prove":
		prove(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `goagents-audit: produce and verify audit proof bundles

  prove  -journal <file> -sth <file> (-tool <id> | -index <n>) [-out <file>]
         build a ProofBundle for one record against a signed tree head

  verify -bundle <file> -pubkey <hex|file>
         verify a ProofBundle offline; exit 0 if authentic, 1 otherwise

Export a journal for `+"`prove`"+` with: json.Marshal(store.History(ctx, runID)).
`)
	os.Exit(2)
}

// staticStore is a read-only Durable backed by an exported journal, so the CLI can reuse the
// audit package's proof builders without a live store.
type staticStore struct{ recs []agent.Record }

func (s staticStore) History(context.Context, string) ([]agent.Record, error) { return s.recs, nil }
func (staticStore) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("goagents-audit: journal is read-only")
}

func prove(args []string) {
	fs := flagSet("prove")
	journal := fs.String("journal", "", "path to the exported journal JSON ([]Record)")
	sthPath := fs.String("sth", "", "path to the signed tree head JSON")
	tool := fs.String("tool", "", "prove the tool call with this ToolUseID")
	index := fs.Int("index", -1, "prove the record at this journal index")
	out := fs.String("out", "", "write the bundle here (default: stdout)")
	_ = fs.Parse(args)

	if *journal == "" || *sthPath == "" || (*tool == "" && *index < 0) {
		usage()
	}

	var recs []agent.Record
	readJSON(*journal, &recs)
	var sth audit.SignedTreeHead
	readJSON(*sthPath, &sth)

	store := staticStore{recs: recs}
	var (
		bundle audit.ProofBundle
		err    error
	)
	if *tool != "" {
		bundle, err = audit.ProveToolCall(context.Background(), store, "", *tool, sth)
	} else {
		bundle, err = audit.ProveRecord(context.Background(), store, "", *index, sth)
	}
	if err != nil {
		fatal(err)
	}

	b, _ := json.MarshalIndent(bundle, "", "  ")
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
}

func verify(args []string) {
	fs := flagSet("verify")
	bundlePath := fs.String("bundle", "", "path to the ProofBundle JSON")
	pubkey := fs.String("pubkey", "", "ed25519 public key as hex, or a path to a file containing it")
	_ = fs.Parse(args)

	if *bundlePath == "" || *pubkey == "" {
		usage()
	}

	var bundle audit.ProofBundle
	readJSON(*bundlePath, &bundle)
	pub := readPubKey(*pubkey)

	ok, err := bundle.Verify(pub)
	if err != nil {
		fatal(err)
	}
	if !ok {
		fmt.Println("FAIL: proof did not verify under this key")
		os.Exit(1)
	}
	fmt.Printf("OK: run %q record verified in a signed tree of size %d\n", bundle.RunID, bundle.STH.Size)
}

// readPubKey accepts a hex string directly, or a path to a file whose (trimmed) contents are
// hex. The key must come from out-of-band; that is the whole point of the trust model.
func readPubKey(s string) []byte {
	raw := s
	if b, err := os.ReadFile(s); err == nil {
		raw = string(b)
	}
	raw = trimSpace(raw)
	key, err := hex.DecodeString(raw)
	if err != nil {
		fatal(fmt.Errorf("public key must be hex (or a file of hex): %w", err))
	}
	return key
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		fatal(fmt.Errorf("parse %s: %w", path, err))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
