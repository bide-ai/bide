// The flowedit module holds the flow editor's engine (docs/design/visual-editor.md). It is its own
// module because it reads and edits YAML, which the root module (no dependencies) must not require.
module github.com/bide-ai/bide/flowedit

go 1.27.0

require (
	github.com/bide-ai/bide v0.21.0
	go.yaml.in/yaml/v3 v3.0.5
)
