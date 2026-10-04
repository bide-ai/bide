// Package agent is a stub of bide's pre-1.0 agent API, as the migrate tool's tests need it: the
// signatures the rules rewrite to, which the migrated test cases must compile against.
package agent

import (
	"context"
	"encoding/json"
	"iter"
	"time"
)

type Message struct{ Role string }

func (m Message) Text() string { panic("stub") }

func UserText(s string) Message { panic("stub") }

type Request struct{}
type Stream struct{}

type Model interface {
	Stream(ctx context.Context, req Request) (*Stream, error)
}

type Entry struct {
	Seq  int64
	Name string
	Data []byte
}

type Store interface {
	Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error)
	Get(ctx context.Context, runID, name string) (Entry, bool, error)
	Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error]
}

type Record struct {
	Name   string
	Result json.RawMessage
}

type MemStore struct{}

func NewMemStore() *MemStore { panic("stub") }
func (m *MemStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	panic("stub")
}
func (m *MemStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) { panic("stub") }
func (m *MemStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	panic("stub")
}

type Journal struct{}

func NewJournal(s Store) (*Journal, error)                                     { panic("stub") }
func (j *Journal) History(ctx context.Context, runID string) ([]Record, error) { panic("stub") }
func (j *Journal) Store() Store                                                { panic("stub") }

type Middleware func(next any) any
type ToolMiddleware func(next any) any
type SamplingOption func(*int)

func Temperature(v float64) SamplingOption { panic("stub") }

type ToolChoice struct{ Mode string }
type RunInfo struct{ RunID string }
type Identity struct{ Actor string }
type Waker interface{ Wake() }

type Option interface{ applyAgent() }
type RunOption interface{ applyRun() }
type AgentRunOption interface {
	Option
	RunOption
}

func WithTools(tools ...Tool) Option                     { panic("stub") }
func WithMiddleware(mw ...Middleware) Option             { panic("stub") }
func WithToolMiddleware(mw ...ToolMiddleware) Option     { panic("stub") }
func WithMaxConcurrency(n int) AgentRunOption            { panic("stub") }
func WithSampling(opts ...SamplingOption) AgentRunOption { panic("stub") }
func WithSystemPromptFunc(fn func(ctx context.Context, run RunInfo) (string, error)) Option {
	panic("stub")
}
func WithMaxTurns(n int) AgentRunOption        { panic("stub") }
func WithSystemPrompt(s string) AgentRunOption { panic("stub") }
func WithWaker(w Waker) AgentRunOption         { panic("stub") }
func WithIdentity(id Identity) AgentRunOption  { panic("stub") }
func WithSaga() RunOption                      { panic("stub") }

type OutputMode string

const OutputNative OutputMode = "native"

func WithOutputMode(m OutputMode) RunOption { panic("stub") }

type Agent struct{}

func New(model Model, j *Journal, opts ...Option) (*Agent, error) { panic("stub") }

func (a *Agent) With(opts ...Option) (*Agent, error) { panic("stub") }
func (a *Agent) Run(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error) {
	panic("stub")
}
func (a *Agent) Resume(ctx context.Context, runID string, opts ...RunOption) (*Result, error) {
	panic("stub")
}
func (a *Agent) Stream(ctx context.Context, runID string, input Message, opts ...RunOption) *RunStream {
	panic("stub")
}
func (a *Agent) RunTyped[T any](ctx context.Context, runID string, input Message, opts ...RunOption) (T, *Result, error) {
	panic("stub")
}
func (a *Agent) Session(ctx context.Context, id string) (*Session, error) { panic("stub") }

type Result struct {
	Message Message
	RunID   string
}

type RunEvent interface{ runEvent() }

type RunStream struct{}

func (s *RunStream) Events() iter.Seq[RunEvent] { panic("stub") }
func (s *RunStream) Result() (*Result, error)   { panic("stub") }

type Session struct{}

func (s *Session) Send(ctx context.Context, input Message, opts ...RunOption) (*Result, error) {
	panic("stub")
}

// Tools

type Safety struct{ ReadOnly, Idempotent bool }

type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

type ToolSpec struct {
	Name, Description string
	Input             json.RawMessage
	Safety            Safety
	Timeout           time.Duration
}

type ToolOption interface{ applyTool() }

func WithSafety(s Safety) ToolOption         { panic("stub") }
func WithTimeout(d time.Duration) ToolOption { panic("stub") }

func Func[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) (Tool, error) {
	panic("stub")
}
func MustFunc[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) Tool {
	panic("stub")
}
func MustCompensatedFunc[In, Out any](name, description string, do func(context.Context, In) (Out, error), undo func(context.Context, In, Out) error, opts ...ToolOption) Tool {
	panic("stub")
}
func MustSubAgent(name, description string, sub *Agent, opts ...ToolOption) Tool { panic("stub") }

type Retriever interface{ Retrieve() }

func MustRetrievalTool(name, description string, r Retriever, k int, opts ...ToolOption) Tool {
	panic("stub")
}

// Pauses and verbs

type ApprovalPending struct{ RunID string }
type InterruptPending struct{ RunID string }
type SignalPending struct{ RunID string }
type TimerPending struct{ RunID string }
type OutcomeUnknown struct{ RunID string }

func (e *ApprovalPending) Error() string  { panic("stub") }
func (e *InterruptPending) Error() string { panic("stub") }
func (e *SignalPending) Error() string    { panic("stub") }
func (e *TimerPending) Error() string     { panic("stub") }
func (e *OutcomeUnknown) Error() string   { panic("stub") }

type OpKind string

const (
	OpTool OpKind = "tool"
	OpStep OpKind = "step"
)

type HaltCause string

const HaltCrashed HaltCause = "crashed"

type OpRef struct {
	Kind OpKind
	ID   string
}
type HaltRef struct {
	RunID string
	Op    OpRef
	Cause HaltCause
}
type Outcome struct {
	Result  any
	IsError bool
}
type ResolveOption interface{ applyResolve() }

func ResolveHalt(ctx context.Context, j *Journal, ref HaltRef, out Outcome, opts ...ResolveOption) error {
	panic("stub")
}
func Approve(ctx context.Context, j *Journal, runID, toolUseID string, approved bool) error {
	panic("stub")
}

type StepOption interface{ applyStep() }

func (j *Journal) Step[T any](ctx context.Context, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	panic("stub")
}
func (j *Journal) Signal[T any](ctx context.Context, runID, name string, payload T) error {
	panic("stub")
}
func (j *Journal) Enqueue[T any](ctx context.Context, runID, channel, key string, payload T) error {
	panic("stub")
}
func (j *Journal) AnswerInterrupt[T any](ctx context.Context, runID, name string, value T) error {
	panic("stub")
}
