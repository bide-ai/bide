// Package agent is a stub of bide v0.10's agent API, as the migrate tool's tests need it: the
// signatures the rules rewrite from. Bodies panic; the tests only type-check.
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

type Durable interface {
	Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error)
	History(ctx context.Context, runID string) ([]Record, error)
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
func (m *MemStore) Journal() *Journal { panic("stub") }
func (m *MemStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	panic("stub")
}
func (m *MemStore) History(ctx context.Context, runID string) ([]Record, error) { panic("stub") }

type Journal struct{}

func NewJournal(s Store) (*Journal, error) { panic("stub") }
func (j *Journal) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	panic("stub")
}
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

func WithTools(tools ...Tool) Option         { panic("stub") }
func WithMiddleware(mw ...Middleware) Option { panic("stub") }
func WithSystemPromptFunc(fn func(ctx context.Context, run RunInfo) (string, error)) Option {
	panic("stub")
}
func WithMaxTurns(n int) AgentRunOption        { panic("stub") }
func WithSystemPrompt(s string) AgentRunOption { panic("stub") }
func WithWaker(w Waker) AgentRunOption         { panic("stub") }
func WithSaga() RunOption                      { panic("stub") }

type OutputMode string

const OutputNative OutputMode = "native"

func WithOutputMode(m OutputMode) RunOption { panic("stub") }

type Agent struct{}

// New is v0.10's lenient constructor.
func New(model Model, store Durable, tools ...Tool) *Agent { panic("stub") }

// Build is v0.10's transitional constructor.
func Build(model Model, j *Journal, opts ...Option) (*Agent, error) { panic("stub") }

func (a *Agent) Use(mw ...Middleware) *Agent                                       { panic("stub") }
func (a *Agent) UseTool(mw ...ToolMiddleware) *Agent                               { panic("stub") }
func (a *Agent) WithSampling(opts ...SamplingOption) *Agent                        { panic("stub") }
func (a *Agent) WithSystemPrompt(s string) *Agent                                  { panic("stub") }
func (a *Agent) WithSystemPromptFunc(fn func(context.Context) string) *Agent       { panic("stub") }
func (a *Agent) WithMaxTurns(n int) *Agent                                         { panic("stub") }
func (a *Agent) SetMaxConcurrency(n int) *Agent                                    { panic("stub") }
func (a *Agent) With(opts ...Option) (*Agent, error)                               { panic("stub") }
func (a *Agent) Run(ctx context.Context, runID, input string) (Message, error)     { panic("stub") }
func (a *Agent) RunSaga(ctx context.Context, runID, input string) (Message, error) { panic("stub") }
func (a *Agent) RunResult(ctx context.Context, runID, input string) (*Result, error) {
	panic("stub")
}
func (a *Agent) Stream(ctx context.Context, runID, input string) *AgentStream     { panic("stub") }
func (a *Agent) StreamSaga(ctx context.Context, runID, input string) *AgentStream { panic("stub") }
func (a *Agent) RunMessage(ctx context.Context, runID string, input Message, opts ...RunOption) (*Result, error) {
	panic("stub")
}
func (a *Agent) ResumeRun(ctx context.Context, runID string, opts ...RunOption) (*Result, error) {
	panic("stub")
}
func (a *Agent) RunTypedMessage[T any](ctx context.Context, runID string, input Message, opts ...RunOption) (T, *Result, error) {
	panic("stub")
}
func (a *Agent) Session(ctx context.Context, id string) (*Session, error) { panic("stub") }

func RunTyped[T any](ctx context.Context, a *Agent, runID, input string) (T, error) { panic("stub") }
func RunTypedNative[T any](ctx context.Context, a *Agent, runID, input string) (T, error) {
	panic("stub")
}

type Result struct {
	Message Message
	RunID   string
}

type AgentEvent interface{ agentEvent() }

type AgentStream struct{}

func (s *AgentStream) Events() iter.Seq[AgentEvent] { panic("stub") }
func (s *AgentStream) Final() (Message, error)      { panic("stub") }
func (s *AgentStream) Result() (*Result, error)     { panic("stub") }

type Session struct{}

func (s *Session) Send(ctx context.Context, input string) (Message, error) { panic("stub") }
func (s *Session) SendMessage(ctx context.Context, input Message, opts ...RunOption) (*Result, error) {
	panic("stub")
}

func ContextWithWaker(ctx context.Context, w Waker) context.Context        { panic("stub") }
func ContextWithIdentity(ctx context.Context, id Identity) context.Context { panic("stub") }

// Tools

type Safety struct{ ReadOnly, Idempotent bool }

type Tool interface {
	Name() string
	Description() string
	ArgsSchema() json.RawMessage
	Safety() Safety
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

func SpecOf(t Tool) ToolSpec { panic("stub") }

func Func[In, Out any](name, description string, safety Safety, fn func(context.Context, In) (Out, error), opts ...ToolOption) Tool {
	panic("stub")
}
func CompensatedFunc[In, Out any](name, description string, safety Safety, do func(context.Context, In) (Out, error), undo func(context.Context, In, Out) error, opts ...ToolOption) Tool {
	panic("stub")
}
func SubAgent(name, description string, sub *Agent, opts ...ToolOption) Tool { panic("stub") }

type Retriever interface{ Retrieve() }

func RetrievalTool(name, description string, r Retriever, k int, opts ...ToolOption) Tool {
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

type PendingApproval = ApprovalPending
type Interrupted = InterruptPending
type Awaiting = SignalPending
type Sleeping = TimerPending
type ResumeHalt = OutcomeUnknown

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

func ResolveHaltRef(ctx context.Context, store Durable, ref HaltRef, out Outcome, opts ...ResolveOption) error {
	panic("stub")
}
func ResolveHalt(ctx context.Context, store Durable, runID, toolUseID string, result any, isError bool, opts ...ResolveOption) error {
	panic("stub")
}
func ResolveStepHalt(ctx context.Context, store Durable, runID, name string, result any, isError bool, opts ...ResolveOption) error {
	panic("stub")
}
func Approve(ctx context.Context, d Durable, runID, toolUseID string, approved bool) error {
	panic("stub")
}

type StepOption interface{ applyStep() }

func Step[T any](ctx context.Context, d Durable, runID, name string, fn func(context.Context) (T, error), opts ...StepOption) (T, error) {
	panic("stub")
}
func Signal[T any](ctx context.Context, d Durable, runID, name string, payload T) error {
	panic("stub")
}
func Enqueue[T any](ctx context.Context, d Durable, runID, channel, key string, payload T) error {
	panic("stub")
}
func Send[T any](ctx context.Context, d Durable, runID, channel, key string, payload T) error {
	panic("stub")
}
func AnswerInterrupt[T any](ctx context.Context, d Durable, runID, name string, value T) error {
	panic("stub")
}
func Resume[T any](ctx context.Context, d Durable, runID, key string, value T) error {
	panic("stub")
}

// The scripted model

type ScriptedTurn struct{}
type ScriptedModel struct{}

func (m *ScriptedModel) Stream(ctx context.Context, req Request) (*Stream, error) { panic("stub") }

func NewScriptedModel(turns ...ScriptedTurn) *ScriptedModel { panic("stub") }
func ToolTurn(id, name, args string) ScriptedTurn           { panic("stub") }
func TextTurn(text string) ScriptedTurn                     { panic("stub") }
