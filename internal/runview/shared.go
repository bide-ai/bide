package runview

import (
	"io/fs"

	"github.com/bide-ai/bide/plan"
)

// Assets is the viewer's embedded stylesheets and theme script (style.css, graph.css, extras.css,
// theme.js and the rest of its assets directory), so another page in the module, the flow editor
// (package flowedit), draws with the same tokens and serves them from its own binary. It is
// read-only.
func Assets() fs.FS { return AssetsWith(ThemeSet{}) }

// rawAssets is the embedded directory as it is in the repository, without the generated theme.
func rawAssets() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // "assets" is the embedded directory
	}
	return sub
}

// Sprite is the hidden SVG glyph set a page references with <use href="#ic-NAME"/> (the names an
// Icon, Verdict or node uses). It is built from constants.
func Sprite() string { return string(sprite()) }

// GateText is a declared approval gate in the words the config page, the diff and the flow editor
// share: "2 of 3: ops, finance, risk excluding the requester, times out after 24h" or "one
// decision". It is "" for no gate.
func GateText(g *plan.TopologyGate) string {
	if g == nil {
		return ""
	}
	return topoGateText(g)
}
