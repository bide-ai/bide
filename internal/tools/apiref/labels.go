package apiref

import "strings"

// The stability labels. Their meaning is stated in docs/trust/stability.md.
const (
	Stable        = "stable"
	Experimental  = "experimental"
	BuildingBlock = "building block, not yet used by approval gates"
)

// Labels is the single declared source of the package stability labels, keyed by import path
// relative to the path of the repository's root module (github.com/bide-ai/bide). Render fails when a documented package has no label,
// TestPackageLabels fails when a label names no package, and TestStabilityPage fails when the
// table in docs/trust/stability.md differs from this one.
var Labels = map[string]string{
	"agent":                    Stable,
	"agent/agenttest":          Stable,
	"agent/storetest":          Stable,
	"approvals":                Stable,
	"audit":                    Stable,
	"audit/eventstoretest":     Stable,
	"audit/rfc3161":            BuildingBlock,
	"audit/verify":             Stable,
	"chaos":                    Experimental,
	"codec/gcf":                Stable,
	"eval":                     Experimental,
	"govern":                   Experimental,
	"govern/eventlogtest":      Experimental,
	"govern/postgreslog":       Experimental,
	"govern/redislog":          Experimental,
	"govern/sqlitelog":         Experimental,
	"mcptools":                 Stable,
	"middleware":               Stable,
	"model/anthropic":          Stable,
	"model/gemini":             Stable,
	"model/modeltest":          Stable,
	"model/openai":             Stable,
	"model/provider":           Stable,
	"notify":                   Stable,
	"notify/relay":             Experimental,
	"plan":                     Experimental,
	"plan/planview":            Experimental,
	"schema":                   Stable,
	"signer/awskms":            Stable,
	"signer/azurekv":           Stable,
	"signer/gcpkms":            Stable,
	"signer/pkcs11":            Stable,
	"signer/pkcs11/pkcs11test": Stable,
	"store/postgres":           Stable,
	"store/sqlite":             Stable,
	"trace":                    Stable,
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
// package. The certificate verification core (#313) lives in package audit, which is stable as a
// whole; the approval gates do not yet count a certificate-bound decision.
var SymbolRules = []SymbolRule{
	{Package: "audit", Files: []string{"identity_cert.go", "identity_canonical.go"}, Label: BuildingBlock},
	{Package: "agent", Names: []string{"ReasonNoCert", "ReasonCertChain", "ReasonCertIdentity", "ReasonCertUsage", "ReasonCertWindow"}, Label: BuildingBlock},
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
