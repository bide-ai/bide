// Package mcp adapts a Model Context Protocol server's tools into agent.Tool. Our
// agent is the MCP client/host; an MCP server is a runtime source of tools whose
// schemas are only known at connect time (the untyped json.RawMessage path, not a Go
// struct). Tools() lists a connected session's tools and wraps each one so the agent
// core can call it like any native tool. Built on the official SDK,
// github.com/modelcontextprotocol/go-sdk.
//
// Safety: by default every MCP tool is treated as a side effect (the zero agent.Safety), so it
// runs at most once and halts the run on an unknown-outcome resume rather than risk firing
// twice. MCP tool annotations are hints, and the spec says to treat them as untrusted unless
// they come from a trusted server: a server that labels a destructive tool read-only would
// otherwise have it re-run on resume, retried, and cached. For a server you trust, pass
// TrustAnnotations to Tools and the hints map onto agent.Safety: readOnlyHint is always safe
// to re-run after a crash, and idempotentHint is safe to retry.
//
// Connect optionally wires three more client capabilities (see Option): an
// elicitation resolver (WithElicitation) the server can call to request
// structured input mid-tool-call; a tools-list-changed callback
// (WithToolListChanged) so a long-lived host can re-list when the server's tool
// set changes; and client identification (WithClientInfo). Tools() paginates
// the server's tool list in full (following the opaque cursor), so a server
// that returns its tools across several pages is never silently truncated.
//
// The resolver also answers elicitation embedded in a tool's multi-round-trip
// input requests (SEP-2322), since the SDK routes those to the same handler.
//
// Durability boundary: the resolver answers elicitation live, within the
// interaction that requested it; it is not a durable, resume-across-crash pause.
// Durability is inherited at the tool-call boundary (a tool trusted as read-only or
// idempotent re-runs, and so re-elicits, on resume, while any other tool halts on an
// unknown-outcome resume). For input that must survive a crash and resume in a
// fresh process, use the native agent.Interrupt in a native tool.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bide-ai/bide/agent"
)

// Tools lists the tools exposed by a connected MCP client session and returns each one
// wrapped as an agent.Tool. The session must already be connected (see Connect). The
// returned tools call back through the session, so keep it open for their lifetime.
// Each tool is a side effect unless TrustAnnotations is passed (see the package doc).
//
// The server's tool list is untrusted input. Tools refuses the whole list, with an error
// wrapping agent.ErrProtocol, if a tool's name is outside the MCP grammar (1 to 128 of A-Z a-z
// 0-9 _ - .), two tools share a name, a tool's input schema is not an object schema, or a
// tool's description is longer than the limit (DefaultMaxDescriptionBytes unless set with
// WithMaxDescriptionBytes).
func Tools(ctx context.Context, session *mcp.ClientSession, opts ...ToolsOption) ([]agent.Tool, error) {
	cfg := &toolsConfig{maxResult: DefaultMaxResultBytes, maxDescription: DefaultMaxDescriptionBytes}
	for _, o := range opts {
		o(cfg)
	}
	var tools []agent.Tool
	seen := map[string]bool{}
	// session.Tools follows the pagination cursor internally, so a server that
	// splits its tool list across several pages is listed in full.
	for def, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w (%w)", err, agent.ErrTool)
		}
		if err := checkName(def.Name); err != nil {
			return nil, fmt.Errorf("mcp: list tools: tool %s: %w (%w)", quoteName(def.Name), err, agent.ErrProtocol)
		}
		if seen[def.Name] {
			return nil, fmt.Errorf("mcp: list tools: tool %q is listed twice (%w)", def.Name, agent.ErrProtocol)
		}
		seen[def.Name] = true
		if cfg.maxDescription > 0 && len(def.Description) > cfg.maxDescription {
			return nil, fmt.Errorf("mcp: list tools: tool %q: description is %d bytes, over the %d-byte limit (see WithMaxDescriptionBytes) (%w)",
				def.Name, len(def.Description), cfg.maxDescription, agent.ErrProtocol)
		}
		schema, err := inputSchema(def.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: tool %q: %w (%w)", def.Name, err, agent.ErrProtocol)
		}
		tools = append(tools, &tool{session: session, def: def, schema: schema, cfg: cfg})
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.safety)) {
		if !seen[name] {
			return nil, fmt.Errorf("mcp: WithSafety names tool %q, which the server does not list: %w", name, agent.ErrConfig)
		}
	}
	return tools, nil
}

// checkName reports whether name is a tool name the MCP specification allows: 1 to 128
// characters, each an ASCII letter, a digit, '_', '-' or '.'. The server's names are untrusted
// input that reaches logs, the journal, approval prompts and every model request, so a name with
// a control character, a space, or a Unicode lookalike is refused rather than passed on.
func checkName(name string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("name is %d bytes, want 1 to 128", len(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-' || c == '.') {
			return fmt.Errorf("name has %q, want only letters, digits, '_', '-' and '.'", c)
		}
	}
	return nil
}

// quoteName quotes a server's tool name for an error message, cut to its first 128 bytes.
func quoteName(name string) string {
	if len(name) > 128 {
		return fmt.Sprintf("%q... (%d bytes)", name[:128], len(name))
	}
	return fmt.Sprintf("%q", name)
}

// inputSchema returns a tool's input schema as JSON, which the MCP specification requires to
// be a JSON Schema object of type "object": the schema every provider requires of a tool's
// arguments.
func inputSchema(s any) (json.RawMessage, error) {
	obj, _ := s.(map[string]any) // nil unless the schema is a JSON object
	if obj["type"] != "object" {
		return nil, errors.New(`input schema is not a JSON Schema object of type "object"`)
	}
	return json.Marshal(obj)
}

// ToolsOption configures Tools.
type ToolsOption func(*toolsConfig)

type toolsConfig struct {
	trust          bool                    // map the server's annotations onto Safety (TrustAnnotations)
	timeout        time.Duration           // per-call deadline (WithCallTimeout); 0 = none
	maxResult      int                     // largest result Call accepts, in bytes; <= 0 = no limit
	maxDescription int                     // longest description Tools accepts, in bytes; <= 0 = no limit
	safety         map[string]agent.Safety // per-tool Safety by name (WithSafety)
}

// WithSafety sets the agent.Safety of the server's tool named name, in place of the default
// (a side effect) and of anything its annotations say, trusted or not. It is how an MCP tool
// gets a human approval gate (RequiresApproval, or an m-of-n Approval policy), or is declared
// retry-safe (ReadOnly, Idempotent, IdempotencyKey) by the host rather than by the server.
// Tools fails with agent.ErrConfig if the server does not list a tool of that name, so a
// misspelt gate never leaves the real tool ungated. A later WithSafety for the same name wins.
func WithSafety(name string, s agent.Safety) ToolsOption {
	return func(c *toolsConfig) {
		if c.safety == nil {
			c.safety = map[string]agent.Safety{}
		}
		c.safety[name] = s
	}
}

// DefaultMaxResultBytes is the largest tool result, in bytes of JSON, that Call accepts unless
// WithMaxResultBytes says otherwise: 1 MiB. A result is journaled and sent back to the model on
// every later turn of the run. 1 MiB of text is about 250,000 tokens, more than most models'
// whole context window, so a larger result cannot be used as it is; refusing it keeps one
// server from filling the journal and every later request.
const DefaultMaxResultBytes = 1 << 20

// DefaultMaxDescriptionBytes is the longest tool description, in bytes, that Tools accepts
// unless WithMaxDescriptionBytes says otherwise: 8 KiB. A description is sent to the model with
// every request, for every tool, so its size is paid on every turn of every run, and a long one
// is room for instructions aimed at the model. Descriptions written for a model to read are a
// few sentences to a page; 8 KiB (about 2,000 tokens) leaves room for a detailed one.
const DefaultMaxDescriptionBytes = 8 << 10

// WithCallTimeout bounds each call to a tool from Tools by d, on top of the caller's context.
// There is no default: without it a call waits as long as the run's context allows. A call
// that times out has an unknown outcome (the server may still be running it), so it fails
// with agent.ErrToolOutcomeUnknown, and a side effect halts on resume rather than run again.
// d <= 0 sets no timeout.
func WithCallTimeout(d time.Duration) ToolsOption { return func(c *toolsConfig) { c.timeout = d } }

// WithMaxResultBytes sets the largest result, in bytes of JSON, that a tool's Call accepts
// (DefaultMaxResultBytes by default). A larger result is refused with an error wrapping
// ErrResultTooLarge, never truncated: the tool ran, so the error is a definite failure that says
// so, and the model is not told the call did not happen. n <= 0 removes the limit.
func WithMaxResultBytes(n int) ToolsOption { return func(c *toolsConfig) { c.maxResult = n } }

// WithMaxDescriptionBytes sets the longest tool description, in bytes, that Tools accepts
// (DefaultMaxDescriptionBytes by default). A server that lists a longer one fails Tools with an
// error wrapping agent.ErrProtocol, never a truncated description. n <= 0 removes the limit.
func WithMaxDescriptionBytes(n int) ToolsOption {
	return func(c *toolsConfig) { c.maxDescription = n }
}

// ErrResultTooLarge is a tool call whose result was larger than the limit (see
// WithMaxResultBytes). The tool ran; only its result was refused. It wraps agent.ErrTool.
var ErrResultTooLarge = fmt.Errorf("mcp: tool result too large: %w", agent.ErrTool)

// TrustAnnotations maps the server's tool annotations onto agent.Safety: readOnlyHint becomes
// ReadOnly and idempotentHint becomes Idempotent. Pass it only for a server you trust to label
// its tools correctly; a mislabelled side effect would be re-run, retried, or cached.
func TrustAnnotations() ToolsOption { return func(c *toolsConfig) { c.trust = true } }

// ElicitFunc answers an MCP server's elicitation/create request: the server asks
// the host for structured input (a message plus a JSON schema), either as a
// standalone request or embedded in a tool's multi-round-trip input requests.
// Return an *mcp.ElicitResult with Action "accept" (and Content matching the
// requested schema), "decline", or "cancel". See the package doc for the
// durability boundary: the resolver answers live, it is not a durable pause.
type ElicitFunc func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)

// DeclineElicitation is a safe default resolver that declines every
// elicitation. Use it when the host has no way to gather the requested input.
func DeclineElicitation(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return &mcp.ElicitResult{Action: "decline"}, nil
}

type config struct {
	name, version string
	elicit        ElicitFunc
	onToolsChange func(context.Context)
}

// Option configures the MCP client that Connect builds.
type Option func(*config)

// WithElicitation registers a resolver for the server's elicitation/create
// requests. Setting it advertises the elicitation capability to the server.
func WithElicitation(f ElicitFunc) Option { return func(c *config) { c.elicit = f } }

// WithToolListChanged registers a callback fired when the server notifies that
// its tool list changed. The host should re-call Tools on the session to pick up
// the new set: the agent core takes a fixed tool set per run, so a changed set
// applies to the next run the host builds, not to one already in flight.
func WithToolListChanged(f func(context.Context)) Option {
	return func(c *config) { c.onToolsChange = f }
}

// WithClientInfo overrides the client name and version reported to the server
// (default "bide"/"0.1.0").
func WithClientInfo(name, version string) Option {
	return func(c *config) { c.name, c.version = name, version }
}

// Connect is a convenience over the SDK's client + transport: it builds a client and
// opens a session on the given transport (stdio, in-memory, streamable HTTP, ...).
// Options wire optional client capabilities (elicitation, tools-list-changed,
// client identity). Callers who already hold a *mcp.ClientSession can skip this
// and pass it to Tools.
func Connect(ctx context.Context, transport mcp.Transport, opts ...Option) (*mcp.ClientSession, error) {
	cfg := config{name: "bide", version: "0.1.0"}
	for _, o := range opts {
		o(&cfg)
	}
	co := &mcp.ClientOptions{}
	if cfg.elicit != nil {
		co.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return cfg.elicit(ctx, req)
		}
	}
	if cfg.onToolsChange != nil {
		co.ToolListChangedHandler = func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			cfg.onToolsChange(ctx)
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.name, Version: cfg.version}, co)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w (%w)", err, agent.ErrTool)
	}
	return session, nil
}

// tool wraps a single MCP server tool as an agent.Tool. The schema is dynamic (the
// server's InputSchema at connect time), so there is no Go struct behind it.
type tool struct {
	session *mcp.ClientSession
	def     *mcp.Tool
	schema  json.RawMessage // def.InputSchema, checked to be an object schema
	cfg     *toolsConfig
}

func (t *tool) Name() string        { return t.def.Name }
func (t *tool) Description() string { return t.def.Description }

// ArgsSchema returns the MCP tool's InputSchema as raw JSON, checked and encoded once by Tools,
// for the schema/ package to dialectize per provider.
func (t *tool) ArgsSchema() json.RawMessage { return t.schema }

// Safety is the Safety WithSafety set for this tool, if any. Otherwise it is the zero
// agent.Safety (a side effect) unless the tool came from Tools with TrustAnnotations, in which
// case it derives from the MCP tool annotations:
//
//	readOnlyHint   == true -> Safety{ReadOnly: true}   // always safe to re-run
//	idempotentHint == true -> Safety{Idempotent: true} // safe to retry
//	otherwise (destructive or unannotated) -> Safety{} // halts on unknown-outcome resume
//
// ReadOnly wins if both hints are set: a read-only tool has no side effect to
// double-fire. An absent Annotations block is treated as the destructive default per
// the MCP spec, which is the conservative choice for resume.
func (t *tool) Safety() agent.Safety {
	if s, ok := t.cfg.safety[t.def.Name]; ok {
		return s
	}
	a := t.def.Annotations
	if !t.cfg.trust || a == nil {
		return agent.Safety{}
	}
	switch {
	case a.ReadOnlyHint:
		return agent.Safety{ReadOnly: true}
	case a.IdempotentHint:
		return agent.Safety{Idempotent: true}
	default:
		return agent.Safety{}
	}
}

// Call invokes the tool on the MCP server with the raw JSON args and returns the
// result content as raw JSON (the structured content, if the server sent no content). If the server flags the result IsError, the content is
// returned as a Go error so the agent core sees a failure and can self-correct. A call
// that may have run on the server without its answer arriving fails with
// agent.ErrToolOutcomeUnknown (see callError).
func (t *tool) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	params := &mcp.CallToolParams{Name: t.def.Name}
	if len(args) > 0 {
		// Checked here, so that a failure to encode the request is known not to have sent it.
		if !json.Valid(args) {
			return nil, fmt.Errorf("mcp: call tool %q: arguments are not valid JSON (%w)", t.def.Name, agent.ErrToolArgs)
		}
		params.Arguments = args
	}
	if t.cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.cfg.timeout)
		defer cancel()
	}
	res, err := t.session.CallTool(ctx, params)
	if err != nil {
		return nil, callError(t.def.Name, err)
	}
	var result any = res.Content
	if len(res.Content) == 0 && res.StructuredContent != nil {
		// The specification asks a tool that returns structured content to repeat it as text,
		// but does not require it: without the text, the structured content is the result.
		result = res.StructuredContent
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal result of tool %q: %w (%w)", t.def.Name, err, agent.ErrProtocol)
	}
	if max := t.cfg.maxResult; max > 0 && len(out) > max {
		what := "result"
		if res.IsError {
			what = "error result"
		}
		return nil, fmt.Errorf("mcp: tool %q ran, but its %s is %d bytes, over the %d-byte limit: %w",
			t.def.Name, what, len(out), max, ErrResultTooLarge)
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp: tool %q reported error: %s (%w)", t.def.Name, out, agent.ErrTool)
	}
	return out, nil
}

// callError classifies a failed tools/call. The call failed for certain only when the request
// never left the client (the session was already closed) or the server answered it with a
// JSON-RPC error. Any other failure came after the request was
// sent: the connection dropped, the deadline passed, or the answer could not be read. The server
// may have run the tool, so the error wraps agent.ErrToolOutcomeUnknown and the agent does not
// record the call as failed (which would invite the model to run a side effect again).
func callError(name string, err error) error {
	var wire *jsonrpc.Error
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.As(err, &wire) {
		return fmt.Errorf("mcp: call tool %q: %w (%w)", name, err, agent.ErrTool)
	}
	return fmt.Errorf("mcp: call tool %q: %w (%w)", name, err, agent.ErrToolOutcomeUnknown)
}
