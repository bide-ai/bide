package audit_test

import (
	"encoding"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// auditArtifacts is every exported struct type of audit and audit/verify whose values are written
// as JSON: proof artifacts, their parts, the leaves they commit, and the structured verdicts. Every
// type nested in one of them is checked with it.
var auditArtifacts = []any{
	audit.ProofBundle{},
	audit.AbsenceBundle{},
	audit.Absence{},
	audit.Neighbor{},
	audit.Inclusion{},
	audit.Consistency{},
	audit.EventInclusion{},
	audit.CurrentGrantProof{},
	audit.SignedTreeHead{},
	audit.TreeHead{},
	audit.TreeRef{},
	audit.AnchorEntry{},
	audit.Grant{},
	audit.SignedGrant{},
	audit.RunCertificate{},
	audit.PolicyConvergence{},
	audit.RunVerification{},
	audit.VerifiedPolicy{},
	audit.EvidencePackage{},
	audit.EvidenceAction{},
	audit.EvidenceGrants{},
	audit.EvidenceConsistency{},
	audit.EvidenceReport{},
	audit.EvidenceItem{},
	audit.PolicyContent{},
	audit.ConvergenceContent{},
	audit.ApprovalVerdict{},
	audit.IgnoredDecision{},
	verify.TreeRef{},
}

// notArtifacts is every other exported struct type of audit and audit/verify, with why it is never
// written as JSON. A new exported struct type must be added to one list or the other.
var notArtifacts = map[string]string{
	"audit.AuditedStore":    "a store wrapper; its state is unexported",
	"audit.MemAnchorLog":    "an in-memory log; its state is unexported",
	"audit.MemEventStore":   "an in-memory store; its state is unexported",
	"audit.EventLog":        "an in-memory log; its state is unexported",
	"audit.EarnedAuthority": "a running ledger; its state is unexported",
	"audit.KeySet":          "a key-set definition that carries a func",
	"audit.RunCertSpec":     "the caller's input to CertifyRun",
	"audit.Ed25519Signer":   "a signing key",
	"audit.Ed25519Verifier": "a verifying key",
	"audit.MLDSASigner":     "a signing key",
	"audit.MLDSAVerifier":   "a verifying key",
	"audit.HybridSigner":    "a pair of signing keys",
	"audit.HybridVerifier":  "a pair of verifying keys",
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// Every exported field of every audit JSON artifact, and of every struct nested in one, has an
// explicit snake_case json name, so an artifact's JSON never mixes Go field names with snake_case.
func TestAuditArtifactsUseSnakeCaseJSONNames(t *testing.T) {
	seen := map[reflect.Type]bool{}
	for _, a := range auditArtifacts {
		checkJSONNames(t, reflect.TypeOf(a), seen)
	}
}

func checkJSONNames(t *testing.T, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	if typ.Implements(jsonMarshaler) || reflect.PointerTo(typ).Implements(jsonMarshaler) ||
		typ.Implements(textMarshaler) || reflect.PointerTo(typ).Implements(textMarshaler) {
		return // it writes its own JSON
	}
	for f := range typ.Fields() {
		if !f.IsExported() {
			continue
		}
		tag, hasTag := f.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		switch {
		case f.Anonymous && !hasTag:
			// An untagged embedded struct is flattened into its parent: its fields are the parent's.
			checkJSONNames(t, f.Type, seen)
			continue
		case !hasTag:
			t.Errorf("%s.%s has no json tag, so its JSON name is the Go name %q", typ, f.Name, f.Name)
		case name == "-":
			continue
		case !snakeCase.MatchString(name):
			t.Errorf("%s.%s has json name %q, not snake_case", typ, f.Name, name)
		}
		checkJSONNames(t, f.Type, seen)
	}
}

// Every exported struct type of audit and audit/verify is classified: a JSON artifact checked above,
// or a type that is never written as JSON. A new type cannot skip the check by being forgotten.
func TestAuditArtifactListIsComplete(t *testing.T) {
	listed := map[string]bool{}
	for _, a := range auditArtifacts {
		listed[reflect.TypeOf(a).String()] = true
	}
	for name := range notArtifacts {
		if listed[name] {
			t.Errorf("%s is listed both as an artifact and as not one", name)
		}
		listed[name] = true
	}
	var found []string
	for pkg, dir := range map[string]string{"audit": ".", "verify": "verify"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, s := range gd.Specs {
					ts := s.(*ast.TypeSpec)
					if _, isStruct := ts.Type.(*ast.StructType); isStruct && ts.Name.IsExported() {
						found = append(found, pkg+"."+ts.Name.Name)
					}
				}
			}
		}
	}
	slices.Sort(found)
	if len(found) == 0 {
		t.Fatal("found no exported struct types; the source scan is broken")
	}
	for _, name := range found {
		if !listed[name] {
			t.Errorf("exported struct type %s is in neither auditArtifacts nor notArtifacts", name)
		}
		delete(listed, name)
	}
	for name := range listed {
		t.Errorf("%s is listed but is not an exported struct type of audit or audit/verify", name)
	}
}
