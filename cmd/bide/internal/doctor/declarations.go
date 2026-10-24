package doctor

import (
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/internal/declarations"
	"github.com/bide-ai/bide/internal/term"
)

// readDeclarations reads the declarations document in the file, strictly.
func readDeclarations(path string) (declarations.Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return declarations.Document{}, err
	}
	defer f.Close()
	d, err := declarations.Decode(f)
	if err != nil {
		return declarations.Document{}, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// reportDeclarations prints the findings about the declarations in the file and returns the exit status:
// 0 for a document that reads (a finding is a note, not a failure), 1 for one that does not.
func reportDeclarations(stdout, stderr io.Writer, st term.Style, path string) int {
	d, err := readDeclarations(path)
	if err != nil {
		fmt.Fprintf(stderr, "%sbide doctor: %v\n", st.ErrorPrefix(), err)
		return 1
	}
	policy := "no taint policy"
	switch {
	case d.Policy == nil:
	case d.Policy.Refuse:
		policy = "taint policy: refuse a tainted turn's side effects"
	default:
		policy = "taint policy: an escalation gate"
	}
	fmt.Fprintf(stdout, "%s  %d tool(s)  digest %s\n", policy, len(d.Tools), orNone(d.Digest))
	findings := d.Findings()
	for _, f := range findings {
		fmt.Fprintf(stdout, "%s  %-18s %s\n", st.Warn("note"), f.Subject, f.Message)
	}
	if len(findings) == 0 {
		fmt.Fprintf(stdout, "%s  nothing to look at\n", st.OK("ok  "))
	}
	return 0
}

// reportDiff prints the changes from the old document to the new and returns the exit status: 1 when
// anything is weakened, or when either document does not read or the two do not agree with their digests;
// 0 otherwise.
func reportDiff(stdout, stderr io.Writer, st term.Style, oldPath, newPath string) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%sbide doctor: %v\n", st.ErrorPrefix(), err)
		return 1
	}
	oldDoc, err := readDeclarations(oldPath)
	if err != nil {
		return fail(err)
	}
	newDoc, err := readDeclarations(newPath)
	if err != nil {
		return fail(err)
	}
	changes, err := declarations.Diff(oldDoc, newDoc)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "digest %s -> %s\n", orNone(oldDoc.Digest), orNone(newDoc.Digest))
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "%s  no difference\n", st.OK("ok  "))
		return 0
	}
	weakened := 0
	for _, class := range []declarations.Class{declarations.Weakened, declarations.Changed, declarations.Strengthened} {
		for _, c := range changes {
			if c.Class != class {
				continue
			}
			label := st.Dim("changed     ")
			switch class {
			case declarations.Weakened:
				weakened++
				label = st.Fail("WEAKENED    ")
			case declarations.Strengthened:
				label = st.OK("strengthened")
			}
			note := ""
			if !c.Covered {
				note = st.Dim("  (not in the digest)")
			}
			fmt.Fprintf(stdout, "%s  %s: %s%s\n", label, c.Subject, c.What, note)
		}
	}
	if weakened > 0 {
		fmt.Fprintf(stdout, "\n%d change(s) weaken the declarations\n", weakened)
		return 1
	}
	return 0
}

func orNone(digest string) string {
	if digest == "" {
		return "(none)"
	}
	return digest[:min(12, len(digest))]
}
