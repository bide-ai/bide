package apiref

import "strings"

// The stability labels. Their meaning is stated in docs/trust/stability.md.
const (
	Core          = "core"
	Experimental  = "experimental"
	BuildingBlock = "building block, not yet used by approval gates"
)

// Labels is the single declared source of the package stability labels, keyed by import path
// relative to the path of the repository's root module (github.com/bide-ai/bide). Render fails when a documented package has no label,
// TestPackageLabels fails when a label names no package, and TestStabilityPage fails when the
// table in docs/trust/stability.md differs from this one.
var Labels = map[string]string{
	"agent":                    Core,
	"agent/agenttest":          Core,
	"agent/storetest":          Core,
	"anchor/awss3":             Experimental,
	"anchor/awss3/awss3test":   Experimental,
	"approvals":                Core,
	"audit":                    Core,
	"audit/anchorobj":          Experimental,
	"audit/crlrefresh":         Experimental,
	"audit/eventstoretest":     Core,
	"audit/rfc3161":            BuildingBlock,
	"audit/verify":             Core,
	"chaos":                    Experimental,
	"codec/gcf":                Core,
	"eval":                     Experimental,
	"flowedit":                 Experimental,
	"govern":                   Experimental,
	"govern/eventlogtest":      Experimental,
	"govern/postgreslog":       Experimental,
	"govern/redislog":          Experimental,
	"govern/sqlitelog":         Experimental,
	"mcptools":                 Core,
	"middleware":               Core,
	"model/anthropic":          Core,
	"model/gemini":             Core,
	"model/modeltest":          Core,
	"model/openai":             Core,
	"model/provider":           Core,
	"notify":                   Core,
	"notify/relay":             Experimental,
	"plan":                     Experimental,
	"plan/planview":            Experimental,
	"schema":                   Core,
	"signer/awskms":            Core,
	"signer/azurekv":           Core,
	"signer/gcpkms":            Core,
	"signer/pkcs11":            Core,
	"signer/pkcs11/pkcs11test": Core,
	"store/postgres":           Core,
	"store/sqlite":             Core,
	"trace":                    Core,
}

// A Config is the label tables a page is rendered with.
type Config struct {
	Labels map[string]string
	Rules  []SymbolRule
}

// Default is the repository's own configuration.
var Default = Config{Labels: Labels, Rules: SymbolRules}

// A SymbolRule labels symbols of a package whose label is a different one: the symbols declared
// in Files, and the symbols called Names (or the methods of a type so called).
type SymbolRule struct {
	Package string // import path relative to the core module
	Files   []string
	Names   []string
	Label   string
}

// SymbolRules label the parts of a package that are further along, or less far along, than the
// package. The kill switch, accountable erasure (redaction) and legal holds are experimental
// inside the core packages: their API may change in a minor release while the audit guarantees
// they state hold (docs/trust/stability.md).
var SymbolRules = []SymbolRule{
	{
		Package: "agent",
		// redact.go is the tool-error URL redactor (core) and redaction.go also holds the tombstone
		// format (LeafHash, NewTombstone, Tombstone*), which is part of the store contract and stays
		// core, so redaction.go is labelled by name.
		Files: []string{"hold.go", "notify_stop.go", "redact_run.go",
			"stop_control.go", "stop_hold.go", "stop_policy.go", "stop_record.go", "stop_switch.go", "stop_trail.go"},
		Names: []string{"WithRecoverStop", "RunEnded", "Redactor", "Compactor", "CompactionReport", "RedactState",
			"ErrRedactNotTerminal", "ErrRedactLeased", "ErrRedactProtected", "ErrRedactMismatch", "ErrRedactSession",
			"ErrRedactParentLive", "ErrRedactChanged", "ErrRedactNoEntry", "RedactionProtected", "RedactionEndMarkers",
			"CheckRedaction", "RedactionAncestors", "RedactionSession"},
		Label: Experimental,
	},
	{
		Package: "audit",
		Files:   []string{"hold_authorization.go", "redaction_trail.go", "stop_trail.go", "stop_verify.go"},
		Names:   []string{"ErrRedacted", "NoteRedacted"},
		Label:   Experimental,
	},
}

// labelOf is the label of a symbol: a matching rule's, else the package's.
func (c Config) labelOf(pkg *Package, name, file string) string {
	for _, r := range c.Rules {
		if r.Package != pkg.Rel {
			continue
		}
		for _, f := range r.Files {
			if f == file {
				return r.Label
			}
		}
		for _, n := range r.Names {
			if n == name || strings.HasPrefix(name, n+".") {
				return r.Label
			}
		}
	}
	return pkg.Label
}
