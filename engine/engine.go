// Package engine is the headless core: the session loop that streams model
// turns, executes tool calls, and appends everything to the session's
// message history. Every frontend (CLI, TUI, server) is a client of this
// package; none of them are imported by it.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// Hooks is the slice of the plugin host the engine uses. *plugin.Host
// satisfies it; tests use fakes. A nil Hooks disables all hook dispatch.
type Hooks interface {
	ChatParams(ctx context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams
	SystemTransform(ctx context.Context, req *plugin.SystemTransformRequest) []string
	ShellEnv(ctx context.Context, req *plugin.ShellEnvRequest) map[string]string
	ToolExecuteBefore(ctx context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string)
	ToolExecuteAfter(ctx context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts
	ExecuteTool(ctx context.Context, req *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error)
	Emit(events []plugin.Event)
	Tools() []plugin.ToolDef
}

// Tool is a built-in (in-process) tool.
type Tool struct {
	Def provider.ToolDef
	Run func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error)
}

// Event is one entry in the session's event stream. Event types follow ACP
// naming where a choice is arbitrary (see AGENTS.md).
type Event struct {
	Type       string              `json:"type"`
	SessionID  string              `json:"session_id"`
	Text       string              `json:"text,omitempty"`
	Message    *message.Message    `json:"message,omitempty"`
	ToolCall   *message.ToolCall   `json:"tool_call,omitempty"`
	Output     message.Parts       `json:"output,omitempty"`
	IsError    bool                `json:"is_error,omitempty"`
	Usage      *provider.Usage     `json:"usage,omitempty"`
	StopReason provider.StopReason `json:"stop_reason,omitempty"`

	// Goal-loop fields (set on goal.* events; see goal.go and the state
	// machine documented atop goal.go). GoalCondition is carried by
	// goal.set; GoalReason/GoalMet/GoalTurn by goal.eval; GoalReason/GoalTurn
	// by goal.stalled (GoalAttempt is the 1-based retry attempt);
	// GoalReason/GoalTurns by goal.achieved; goal.cleared carries GoalReason
	// when it was triggered by a permanently-failing worker turn, empty for
	// an ordinary caller-initiated clear.
	//
	// GoalRetryable/GoalRetryableClass/GoalWaiting are also carried by
	// goal.stalled (see GitHub issue #61 and promptTurnWithRetry):
	// GoalRetryable is true when the failure was classified provider-
	// retryable weather (provider.RetryableError) rather than a
	// deterministic failure; GoalRetryableClass names the classification
	// (provider.RetryableClass — overloaded/rate_limited/server_error);
	// GoalWaiting is true while still within the retryable budget (still
	// "waiting out provider weather") and false on the final stalled record
	// that reports the budget exhausted (the turn is about to park, not
	// die — see PursueGoal's doc comment). All three are zero-valued on a
	// deterministic-path stall, unchanged from before they existed.
	GoalCondition      string `json:"goal_condition,omitempty"`
	GoalReason         string `json:"goal_reason,omitempty"`
	GoalMet            bool   `json:"goal_met,omitempty"`
	GoalTurn           int    `json:"goal_turn,omitempty"`
	GoalTurns          int    `json:"goal_turns,omitempty"`
	GoalAttempt        int    `json:"goal_attempt,omitempty"`
	GoalRetryable      bool   `json:"goal_retryable,omitempty"`
	GoalRetryableClass string `json:"goal_retryable_class,omitempty"`
	GoalWaiting        bool   `json:"goal_waiting,omitempty"`

	// Question fields, carried by question.asked (see ask_user.go and
	// docs/design/question-tool.md). QuestionCallID is the ask_user tool
	// call's own CallID; QuestionItems is the batch it asked.
	QuestionCallID string                `json:"question_call_id,omitempty"`
	QuestionItems  []plugin.QuestionItem `json:"question_items,omitempty"`
}

// Event types.
const (
	EventTextDelta      = "text.delta"
	EventReasoningDelta = "reasoning.delta"
	EventMessage        = "message"
	EventToolStart      = "tool.start"
	EventToolEnd        = "tool.end"

	// Goal-loop events (see goal.go).
	EventGoalSet      = "goal.set"
	EventGoalEval     = "goal.eval"
	EventGoalStalled  = "goal.stalled"
	EventGoalAchieved = "goal.achieved"
	EventGoalCleared  = "goal.cleared"

	// Question events (see ask_user.go and docs/design/question-tool.md).
	EventQuestionAsked    = "question.asked"
	EventQuestionAnswered = "question.answered"
)

// Config configures a Session.
type Config struct {
	Providers provider.Registry
	Model     message.ModelRef // initial model; swap any time with SetModel
	System    []string         // base system prompt segments
	MaxTokens int              // per-response cap; defaults to 8192
	WorkDir   string           // working directory for built-in tools

	// SessionDir is where session logs are persisted, one JSONL file per
	// session. Empty disables persistence entirely.
	SessionDir string

	Hooks Hooks // optional plugin host
	// OnEvent is optional; called synchronously, keep it fast. The goal.*
	// events (see goal.go) are emitted while Session.mu is held so the event
	// stream can never invert relative to the journaled log order under a
	// concurrent ClearGoal/evaluation race — so OnEvent must NEVER call back
	// into the Session that raised the event (Prompt, ClearGoal, ActiveGoal,
	// etc.), which would deadlock on that same mutex.
	OnEvent func(Event)

	// OnRequest, when non-nil, is invoked synchronously in streamTurn with the
	// exact final request about to be sent to the provider — after params,
	// system-segment, and hook assembly, immediately before prov.Stream. turn
	// counts from 1 for the first model call of the session and increments once
	// per model call, so a tool loop advances it. The *provider.Request is
	// SHARED with the provider call: callbacks MUST NOT mutate it or anything it
	// references (System, Messages, Tools). A nil OnRequest costs nothing.
	OnRequest func(turn int, req *provider.Request)

	// Instructions controls project-instruction (AGENTS.md) injection into
	// the system prompt. A nil value is the default: auto-discover AGENTS.md
	// by walking up from WorkDir. See InstructionsConfig.
	Instructions *InstructionsConfig

	// SkillsDirs are the directories scanned for Agent Skills
	// (agentskills.io), each holding skill subdirectories with a SKILL.md.
	// A nil value is the default: use <WorkDir>/.agents/skills when that
	// directory exists. An explicit empty (non-nil) slice disables skill
	// discovery entirely. Duplicate skill names across dirs are an error.
	// See skills.go.
	SkillsDirs []string

	// MCP is the MCP client integration this session's tools draw from: its
	// Tools() are merged into the request's tool list (namespaced
	// mcp__<server>__<tool>) and a call to one of them routes through
	// CallTool. *MCPManager (see mcp.go) is the production implementation,
	// built once per process and shared across every session — nil (the
	// default) registers no MCP tools at all. See MCPRegistry's doc
	// comment for why this is injected rather than built from raw server
	// specs here.
	MCP MCPRegistry

	// Tools are additional built-in tools. The bash tool is always
	// installed.
	Tools       []Tool
	BashTimeout time.Duration // defaults to 2m

	// BashOutputCap bounds the bytes of combined stdout+stderr the bash tool
	// keeps from one command, truncating (head + tail, marker in between)
	// before the output ever reaches the message log. Zero/negative means
	// the default (see defaultBashOutputCap in bash.go, 96KB) — a runaway
	// command (an apt-get/npm install storm is the real-world trigger) can
	// otherwise dump megabytes into a single message and poison the session.
	BashOutputCap int
}

// Session is one conversation: an in-memory history plus the agent loop.
// Methods are safe for one caller at a time; Prompt must not be called
// concurrently with itself.
type Session struct {
	ID string

	cfg   Config
	tools map[string]Tool

	mu        sync.Mutex
	model     message.ModelRef
	history   []message.Message
	usage     provider.Usage // cumulative, across every turn (see appendWithUsage)
	createdAt time.Time
	// lastUsage/haveLastUsage carry the most recent model turn's own Usage
	// (input/output/cache tokens for that one request), distinct from the
	// cumulative usage field above — GET /session surfaces both (issue #62
	// layer 2) so an orchestrator can see the size of the request that JUST
	// went out, not only the running total. haveLastUsage is false until
	// the session's first completed turn (fresh session, or one reloaded
	// before any turn ever ran against it in any process).
	lastUsage     provider.Usage
	haveLastUsage bool

	logFile        *os.File // session log; nil until first write (see store.go)
	logStarted     bool     // the log file exists on disk
	lastPersistErr error

	// Project-instruction segment, loaded once on the first Prompt (see
	// instructions.go). instrLoaded gates the one-time disk read; instrSeg is
	// the cached system-prompt segment (empty when none); instrErr records a
	// present-but-unusable instructions file so every Prompt fails alike;
	// instrPath is the display path of the source file (empty when none), used
	// by the session_info tool to report instruction provenance.
	instrLoaded bool
	instrSeg    string
	instrErr    error
	instrPath   string

	// turn counts model calls made in this session (from 1); lastSystem holds
	// the system segments assembled for the most recent model call. Both feed
	// OnRequest and the session_info tool (see session_info.go). Guarded by mu.
	turn       int
	lastSystem []string

	// skills is the structured catalog discovered on the first Prompt (name +
	// absolute SKILL.md path), used by the session_info tool. The advertised
	// prompt segment lives in skillsSeg below; this is the same catalog, kept
	// structured so session_info can report skill provenance.
	skills []skillInfo

	// Agent Skills catalog segment, discovered once on the first Prompt (see
	// skills.go). Same load-once-cache-error pattern as instructions:
	// skillsLoaded gates the one-time disk scan, skillsSeg is the cached
	// stage-1 catalog (empty when none), skillsErr records a discovery
	// failure so every Prompt fails alike.
	skillsLoaded bool
	skillsSeg    string
	skillsErr    error

	// Goal-loop state (see goal.go). goalActive is set while a goal is set but
	// neither achieved nor cleared; goalCondition holds the current goal's
	// completion condition. Restored on LoadSession from the goal.* records in
	// the session log. Guarded by mu.
	goalActive    bool
	goalCondition string

	// Question state (see ask_user.go and docs/design/question-tool.md).
	// awaitingQuestion is set the instant an ask_user call runs and cleared
	// the instant a new prompt arrives (whether a bare prompt_async or one
	// routed through POST /session/{id}/answer); questionCallID names the
	// pending call. Guarded by mu.
	awaitingQuestion bool
	questionCallID   string
	// pendingResumeAnswer/pendingResumeAnswerSet retain the most recently
	// persisted question.answered payload that has not yet been delivered
	// as an ordinary message (see LoadSession's recMessage/recQuestionAnswered
	// cases and PendingResumeAnswer). Guarded by mu.
	pendingResumeAnswer    string
	pendingResumeAnswerSet bool

	// toolExecCount counts tool-call executions across the session's
	// lifetime: incremented once per call to runToolCall that actually
	// invokes a tool (i.e. not one blocked by a tool.execute.before deny),
	// whether the tool succeeds or returns an error result. The goal loop
	// (see goal.go's promptTurnWithRetry) snapshots this before and after
	// each worker-turn attempt to detect whether a failed attempt executed
	// any tool before failing — a retry re-issues the whole directive, which
	// is unsafe to do blindly once a tool has already run. Guarded by mu.
	toolExecCount int
}

// NewSession creates a session. Nothing touches the network, spawns
// processes, or writes to disk here — provider auth and plugin spawns happen
// on first use, and the session log is created on first message append.
func NewSession(cfg Config) *Session {
	s := newSession(cfg)
	s.ID = newID("ses")
	return s
}

// newSession builds a session minus its ID; NewSession and LoadSession
// share it.
func newSession(cfg Config) *Session {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.BashTimeout <= 0 {
		cfg.BashTimeout = 2 * time.Minute
	}
	s := &Session{
		cfg:       cfg,
		model:     cfg.Model,
		tools:     make(map[string]Tool),
		createdAt: time.Now().UTC(),
	}
	for _, t := range []Tool{bashTool(cfg.BashTimeout, cfg.BashOutputCap), readFileTool(), writeFileTool(), editFileTool(), sessionInfoTool(), askUserTool()} {
		s.tools[t.Def.Name] = t
	}
	for _, t := range cfg.Tools {
		s.tools[t.Def.Name] = t
	}
	return s
}

// SetModel swaps the model for subsequent requests. History transcodes
// automatically; there is no migration step.
func (s *Session) SetModel(ref message.ModelRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref == s.model {
		return
	}
	s.model = ref
	s.persistModel(ref)
}

// Model returns the session's current model.
func (s *Session) Model() message.ModelRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// CreatedAt returns when the session was created (or, for a loaded session,
// when it was originally created per its log header).
func (s *Session) CreatedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt
}

// WorkDir returns the session's working directory: Config.WorkDir for a
// fresh session, or the value restored from the session log header for a
// loaded one (which wins over the Config.WorkDir the caller supplied to
// LoadSession — see store.go). It never changes after construction, so no
// lock is needed (consistent with direct s.cfg.WorkDir reads elsewhere, e.g.
// bash.go and filetools.go).
func (s *Session) WorkDir() string {
	return s.cfg.WorkDir
}

// Usage returns cumulative token usage across all turns.
func (s *Session) Usage() provider.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// LastUsage returns the most recently completed model turn's own Usage
// (that one request's input/output/cache tokens, not the running total —
// see Usage), and whether any turn has completed yet (for this session,
// live or reloaded from its log — see appendWithUsage and LoadSession's
// recMessage case). ok is false for a session that has never completed a
// turn in any process.
func (s *Session) LastUsage() (usage provider.Usage, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsage, s.haveLastUsage
}

// LastActivityAt returns the timestamp of the most recently appended
// message (user, assistant, or tool), or CreatedAt if no message has been
// appended yet.
//
// This exists (issue #62 layer 3) because fleet monitors previously had no
// direct way to answer "is this session still doing something" — they
// sampled Session.Seq (server/journal.go) twice, a session apart, and
// compared, to distinguish quiet progress from a session wedged mid-turn.
// That double-sample is slow, racy against a session legitimately paused
// between polls (e.g. awaiting_input), and only ever answers a RELATIVE
// question ("did anything happen between my two samples"), never an
// absolute one ("how long has this been quiet"). LastActivityAt answers the
// absolute question directly from state the engine already holds: resident
// in memory for a live session, and — because every message record carries
// its own CreatedAt — equally available the moment a non-resident session
// is reloaded from its log, with no extra bookkeeping.
func (s *Session) LastActivityAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) == 0 {
		return s.createdAt
	}
	return s.history[len(s.history)-1].CreatedAt
}

// toolExecutions returns the current tool-execution counter (see
// toolExecCount). It only ever increases, and only when a tool actually
// runs, never when one is blocked by tool.execute.before.
func (s *Session) toolExecutions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolExecCount
}

// History returns a copy of the session's message history.
func (s *Session) History() []message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]message.Message(nil), s.history...)
}

func (s *Session) append(m message.Message) {
	s.appendWithUsage(m, nil)
}

// appendWithUsage is append's usage-carrying variant, used for the one
// assistant message that ends a model turn (see Prompt): usage is the
// provider's per-turn Usage for that message, or nil for every other
// message (user, tool, or an interrupted partial assistant message with no
// completed turn to report). Recording it on the message record itself
// (see store.go's record.Usage) is what makes Session.Usage()/LastUsage()
// survive a process restart — LoadSession sums every record's Usage back
// into cumulative usage and keeps the last one seen as lastUsage, since the
// log has no separate cumulative-usage record to replay instead.
func (s *Session) appendWithUsage(m message.Message, usage *provider.Usage) {
	// Normalize before this message enters history at all: see
	// message.Message.Normalize's doc comment. This is the single ingest
	// choke point every message passes through (user, assistant, and tool
	// messages alike), so it is the right place to scrub a
	// present-but-zero-length Reasoning.ProviderData entry before it can
	// diverge an in-memory history from what LoadSession would reload.
	m.Normalize()
	s.mu.Lock()
	s.history = append(s.history, m)
	if usage != nil {
		s.usage.InputTokens += usage.InputTokens
		s.usage.OutputTokens += usage.OutputTokens
		s.usage.CacheReadTokens += usage.CacheReadTokens
		s.usage.CacheWriteTokens += usage.CacheWriteTokens
		s.lastUsage = *usage
		s.haveLastUsage = true
	}
	s.persistMessage(&m, usage)
	s.mu.Unlock()
}

func (s *Session) emit(ev Event) {
	ev.SessionID = s.ID
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(ev)
	}
}

func (s *Session) emitStatus(status string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(map[string]string{"status": status})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventSessionStatus,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// emitFileEdited notifies plugins that a built-in file tool successfully
// wrote path (absolute).
func (s *Session) emitFileEdited(path string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(plugin.FileEditedProperties{Path: path})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventFileEdited,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// emitToolExecuteStart/emitToolExecuteEnd bracket the actual execution of a
// tool call (built-in or plugin-provided). They do not fire for calls denied
// by tool.execute.before, since those never execute.
func (s *Session) emitToolExecuteStart(tool, callID string) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(plugin.ToolExecuteStartProperties{Tool: tool, CallID: callID})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventToolExecuteStart,
		SessionID:  s.ID,
		Properties: props,
	}})
}

func (s *Session) emitToolExecuteEnd(tool, callID string, ok bool) {
	if s.cfg.Hooks == nil {
		return
	}
	props, _ := json.Marshal(plugin.ToolExecuteEndProperties{Tool: tool, CallID: callID, OK: ok})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventToolExecuteEnd,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// emitSessionError notifies plugins that a prompt/turn terminated with an
// error. The error string is passed through plugin.SanitizeSessionError
// first: it caps the length and best-effort redacts obvious credential
// shapes (bearer tokens, Authorization header values, key=value secrets)
// that provider adapters can embed in wrapped HTTP errors — see
// SanitizeSessionError and PROTOCOL.md. This is best-effort, not a
// guarantee against every possible leak.
//
// context.Canceled is deliberately excluded: a cancelled context is an
// operator-initiated stop (POST /abort, DELETE /goal, server drain), not a
// failure — the server layer draws the same line (runPrompt/runGoal in
// server/handlers.go journal it as session.aborted / a clean stop, never
// session.error). Emitting session.error for every cancellation would be
// noisy and misleading to plugins reacting to it as a real fault.
func (s *Session) emitSessionError(err error) {
	if s.cfg.Hooks == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	props, _ := json.Marshal(plugin.SessionErrorProperties{Message: plugin.SanitizeSessionError(err.Error())})
	s.cfg.Hooks.Emit([]plugin.Event{{
		Type:       plugin.EventSessionError,
		SessionID:  s.ID,
		Properties: props,
	}})
}

// Prompt appends a user message and runs the agent loop — stream a turn,
// execute any tool calls, feed results back — until the model ends its turn.
// It returns the final assistant message.
func (s *Session) Prompt(ctx context.Context, text string) (*message.Message, error) {
	// Load project instructions once, before mutating history: a
	// present-but-unusable AGENTS.md fails the prompt without recording a
	// user message or calling the provider.
	if err := s.ensureInstructions(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	// Discover Agent Skills once, before mutating history: a malformed
	// SKILL.md or a duplicate skill name fails the prompt without recording a
	// user message or calling the provider (see skills.go).
	if err := s.ensureSkills(); err != nil {
		s.emitSessionError(err)
		return nil, err
	}
	// Any new user message — whether a bare prompt_async or one routed
	// through POST /session/{id}/answer's interactive branch — clears a
	// pending ask_user question and persists question.answered, exactly
	// once (design doc §3). This is the single, idempotent owner of that
	// record for the interactive path; the goal-paused /answer branch is
	// the deliberate exception (see AnswerQuestion), and by the time its
	// resumed Prompt call reaches here awaitingQuestion is already false,
	// so this is a no-op then.
	s.clearAwaitingQuestionOnPrompt(text)

	s.append(message.Message{
		ID:        newID("msg"),
		Role:      message.RoleUser,
		Parts:     message.Parts{&message.Text{Text: text}},
		CreatedAt: time.Now().UTC(),
	})
	s.emitStatus("busy")
	defer s.emitStatus("idle")

	for {
		asst, stop, usage, err := s.streamTurn(ctx)
		if err != nil {
			var interrupted *interruptedTurnError
			if errors.As(err, &interrupted) {
				// The turn died after the model already emitted one or
				// more tool_call blocks: append the model's intent and
				// synthetic (never silently dropped) results for it
				// before surfacing the failure, so history stays
				// protocol-valid for every later request build — see
				// interruptedTurnError's doc comment.
				s.append(*interrupted.partial)
				s.emit(Event{Type: EventMessage, Message: interrupted.partial})
				toolMsg := interruptedToolResults(interrupted.partial)
				s.append(toolMsg)
			}
			s.emitSessionError(err)
			return nil, err
		}
		s.appendWithUsage(*asst, &usage)
		s.emit(Event{Type: EventMessage, Message: asst, StopReason: stop, Usage: &usage})

		if stop != provider.StopToolUse {
			return asst, nil
		}
		results := s.runToolCalls(ctx, asst)
		if len(results) == 0 {
			// tool_use stop with no tool calls: treat as end of turn
			// rather than looping forever.
			return asst, nil
		}
		s.append(message.Message{
			ID:        newID("msg"),
			Role:      message.RoleTool,
			Parts:     results,
			CreatedAt: time.Now().UTC(),
		})
		if askUserExecuted(asst) {
			// docs/design/question-tool.md §2: "if any tool call executed
			// this round was ask_user, the turn ends here regardless of
			// stop." Other tool calls batched into the same assistant
			// message already ran and got real results above; only the
			// loop-continuation is skipped. This is an ordinary end of
			// turn, not an error — no session.error, no special stop
			// reason.
			return asst, nil
		}
	}
}

// askUserExecuted reports whether asst's tool calls include ask_user.
func askUserExecuted(asst *message.Message) bool {
	for _, p := range asst.Parts {
		if tc, ok := p.(*message.ToolCall); ok && tc.Name == askUserToolName {
			return true
		}
	}
	return false
}

// streamTurn makes one model call and returns the assembled assistant
// message.
func (s *Session) streamTurn(ctx context.Context) (*message.Message, provider.StopReason, provider.Usage, error) {
	params := plugin.ChatParams{Model: s.Model()}
	system := append([]string(nil), s.cfg.System...)
	// Project instructions sit after the base system prompt and before any
	// hook-contributed segments (see ensureInstructions in instructions.go).
	if seg := s.instructionSegment(); seg != "" {
		system = append(system, seg)
	}
	// The Agent Skills catalog sits after project instructions and, like
	// them, before any hook-contributed segments (see ensureSkills in
	// skills.go).
	if seg := s.skillsSegment(); seg != "" {
		system = append(system, seg)
	}
	if s.cfg.Hooks != nil {
		params = s.cfg.Hooks.ChatParams(ctx, &plugin.ChatParamsRequest{SessionID: s.ID, Params: params})
		if params.Model.IsZero() {
			params.Model = s.Model()
		}
		system = append(system, s.cfg.Hooks.SystemTransform(ctx, &plugin.SystemTransformRequest{
			SessionID: s.ID,
			Model:     params.Model,
		})...)
	}

	prov, err := s.cfg.Providers.For(params.Model)
	if err != nil {
		return nil, "", provider.Usage{}, err
	}

	maxTokens := s.cfg.MaxTokens
	if params.MaxTokens != nil {
		maxTokens = *params.MaxTokens
	}
	req := &provider.Request{
		Model:       params.Model,
		System:      system,
		Messages:    s.History(),
		Tools:       s.toolDefs(ctx),
		Temperature: params.Temperature,
		TopP:        params.TopP,
		MaxTokens:   maxTokens,
	}

	// Record this turn's assembled system for the session_info tool, bump the
	// per-session turn counter, then hand the exact final request to the
	// observer immediately before the provider call. OnRequest must not mutate
	// req (it is shared with prov.Stream below).
	s.mu.Lock()
	s.turn++
	turn := s.turn
	s.lastSystem = append([]string(nil), system...)
	s.mu.Unlock()
	if s.cfg.OnRequest != nil {
		s.cfg.OnRequest(turn, req)
	}

	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return nil, "", provider.Usage{}, err
	}
	defer stream.Close()

	// text and toolCalls accumulate this turn's content as it streams, so
	// that if the stream dies (or otherwise errors) before EventDone, any
	// tool_call already fully emitted is not simply lost — see the
	// EventToolCall case below and interruptedTurnError's doc comment for
	// why.
	var text strings.Builder
	var toolCalls []*message.ToolCall
	for {
		ev, err := stream.Next()
		if err != nil {
			if len(toolCalls) == 0 {
				// No tool call was ever recorded this turn: nothing can
				// be orphaned, so this is an ordinary turn failure —
				// identical to the pre-fix behavior.
				return nil, "", provider.Usage{}, err
			}
			return nil, "", provider.Usage{}, &interruptedTurnError{
				err:     err,
				partial: s.assemblePartial(text.String(), toolCalls),
			}
		}
		switch ev.Type {
		case provider.EventTextDelta:
			text.WriteString(ev.Text)
			s.emit(Event{Type: EventTextDelta, Text: ev.Text})
		case provider.EventReasoningDelta:
			s.emit(Event{Type: EventReasoningDelta, Text: ev.Text})
		case provider.EventToolCall:
			// A complete tool_use/tool_call block: the provider has
			// finished emitting its arguments (see
			// provider/anthropic/anthropic.go's content_block_stop and
			// provider/openaicompat/openaicompat.go's emitToolCalls), but
			// the turn has not reached EventDone yet, so the engine has
			// not (and must not, before the turn completes normally)
			// executed it. Recorded here purely so a later stream death
			// or error still has this call's identity to work with.
			toolCalls = append(toolCalls, ev.ToolCall)
		case provider.EventDone:
			return ev.Message, ev.StopReason, ev.Usage, nil
		}
	}
}

// assemblePartial builds the assistant message streamTurn returns (wrapped
// in an interruptedTurnError) when the stream errors after recording one or
// more tool calls but before EventDone. It mirrors the shape a provider
// adapter's own assemble (e.g. provider/anthropic/anthropic.go's
// stream.assemble) would produce for the same partial content: any
// accumulated text first, then the tool calls in emission order.
func (s *Session) assemblePartial(text string, toolCalls []*message.ToolCall) *message.Message {
	msg := &message.Message{
		ID:        newID("msg"),
		Role:      message.RoleAssistant,
		Model:     s.Model(),
		CreatedAt: time.Now().UTC(),
	}
	if text != "" {
		msg.Parts = append(msg.Parts, &message.Text{Text: text})
	}
	for _, tc := range toolCalls {
		msg.Parts = append(msg.Parts, tc)
	}
	return msg
}

// interruptedTurnErrorText is the Content text of the synthetic, is_error
// tool-role result the engine appends for every tool call recorded in a
// turn that ended abnormally before the engine could execute it (see
// interruptedTurnError). Exported as a constant (not exported from the
// package) so tests can assert on the exact string.
const interruptedTurnErrorText = "interrupted: tool call was never executed because the turn ended abnormally"

// interruptedTurnError is returned by streamTurn in place of the
// underlying stream/provider error when that error arrived after the
// stream had already emitted one or more complete tool_call blocks for the
// in-flight assistant message (via provider.EventToolCall) but before
// EventDone — i.e. before the engine could ever execute those calls.
//
// # Incident ses_01kx48z4rqfkpbwmzfdv1jzeg6
//
// A goal worker turn died with the Anthropic API 400 "tool_use ids were
// found without tool_result blocks immediately after", and every
// subsequent goal-loop retry then failed identically, killing the goal.
// The mechanism: a provider stream died (or the turn otherwise errored)
// after emitting one or more tool_call blocks but before the engine
// executed them. Before this fix, Prompt's error path
// (`if err != nil { return nil, err }`) simply discarded the assembled
// partial message, which sounds safe — nothing entered history — except
// that is exactly backwards from what actually poisons a session: the
// danger here is not a partial message appended without its result (the
// old truncated-Arguments incident's shape), it is that some OTHER call
// path (a provider adapter's own retry, a resumed session replaying a
// partially-journaled turn, a future change to this loop) could append
// such a message without this same care. Recording the tool calls here
// and synthesizing their results immediately — rather than leaving the
// model's already-emitted intent to either vanish or, worse, reappear
// unpaired from some other path later — is what keeps history
// self-consistent at ingest, mirroring the primary fix for the sibling,
// marshal-level incident (see message.Normalize's doc comment, "fix
// (message,engine): truncated ToolCall.Arguments must never poison
// history").
//
// Prompt handles this by appending partial (the assistant message,
// exactly as if the turn had completed with these tool calls) followed
// immediately by a synthetic tool-role message: one is_error ToolResult
// per recorded call, Content interruptedTurnErrorText. This preserves the
// model's visible intent (which tool it was calling, with what arguments)
// while keeping the transcript replayable — every subsequent request
// build sees a ToolCall immediately followed by its ToolResult, exactly
// as every provider wire protocol requires, instead of replaying the
// orphaned tool_use forever. The turn is still a failure: err (unwrapped
// via Unwrap) is what Prompt ultimately returns to its caller, unchanged
// from the caller's point of view — the goal loop's retry-count and
// tool-executed-before-failing bookkeeping (see promptTurnWithRetry) sees
// the same error it always would have, and toolExecCount is NOT
// incremented (these calls never ran), so a retry is exactly as safe as
// it always was for a turn that failed before executing anything.
//
// provider/anthropic/transcode.go and provider/openaicompat/transcode.go
// carry the defense-in-depth counterpart (message.ResolveOrphanToolCalls)
// for histories poisoned by any other producer; see that function's doc
// comment.
type interruptedTurnError struct {
	err     error
	partial *message.Message
}

func (e *interruptedTurnError) Error() string { return e.err.Error() }
func (e *interruptedTurnError) Unwrap() error { return e.err }

// interruptedToolResults builds the synthetic tool-role message Prompt
// appends immediately after an interruptedTurnError's partial assistant
// message: one is_error ToolResult per ToolCall part in partial, in order.
//
// Like every tool-result append, this message is persisted without an
// EventMessage emit — and since the interrupted calls never executed, no
// EventToolEnd fired either. A pure event-stream consumer therefore sees
// the partial assistant message with no following results for the
// interrupted calls until it reloads history (GET /message, LoadSession).
func interruptedToolResults(partial *message.Message) message.Message {
	var results message.Parts
	for _, p := range partial.Parts {
		tc, ok := p.(*message.ToolCall)
		if !ok {
			continue
		}
		results = append(results, &message.ToolResult{
			CallID:  tc.CallID,
			Content: message.Parts{&message.Text{Text: interruptedTurnErrorText}},
			IsError: true,
		})
	}
	return message.Message{
		ID:        newID("msg"),
		Role:      message.RoleTool,
		Parts:     results,
		CreatedAt: time.Now().UTC(),
	}
}

// toolDefs merges built-in tools, MCP-provided tools, and plugin-provided
// ones.
func (s *Session) toolDefs(ctx context.Context) []provider.ToolDef {
	var defs []provider.ToolDef
	for _, t := range s.tools {
		defs = append(defs, t.Def)
	}
	if s.cfg.MCP != nil {
		defs = append(defs, s.cfg.MCP.Tools(ctx)...)
	}
	if s.cfg.Hooks != nil {
		for _, d := range s.cfg.Hooks.Tools() {
			defs = append(defs, provider.ToolDef{
				Name:        d.Name,
				Description: d.Description,
				InputSchema: d.InputSchema,
			})
		}
	}
	return defs
}

// runToolCalls executes every tool call in an assistant message, in order,
// and returns the ToolResult parts.
func (s *Session) runToolCalls(ctx context.Context, asst *message.Message) message.Parts {
	var results message.Parts
	for _, p := range asst.Parts {
		tc, ok := p.(*message.ToolCall)
		if !ok {
			continue
		}
		out, isErr := s.runToolCall(ctx, tc)
		results = append(results, &message.ToolResult{
			CallID:  tc.CallID,
			Content: out,
			IsError: isErr,
		})
	}
	return results
}

func (s *Session) runToolCall(ctx context.Context, tc *message.ToolCall) (message.Parts, bool) {
	s.emit(Event{Type: EventToolStart, ToolCall: tc})

	args := tc.Arguments
	if s.cfg.Hooks != nil {
		newArgs, deny := s.cfg.Hooks.ToolExecuteBefore(ctx, &plugin.ToolExecuteBeforeRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args,
		})
		if deny != "" {
			out := message.Parts{&message.Text{Text: deny}}
			s.emit(Event{Type: EventToolEnd, ToolCall: tc, Output: out, IsError: true})
			return out, true
		}
		if newArgs != nil {
			args = newArgs
		}
	}

	s.emitToolExecuteStart(tc.Name, tc.CallID)
	s.mu.Lock()
	s.toolExecCount++
	s.mu.Unlock()

	out, isErr := s.executeTool(ctx, tc, args)
	s.emitToolExecuteEnd(tc.Name, tc.CallID, !isErr)

	if s.cfg.Hooks != nil {
		out = s.cfg.Hooks.ToolExecuteAfter(ctx, &plugin.ToolExecuteAfterRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args, Output: out,
		})
	}
	s.emit(Event{Type: EventToolEnd, ToolCall: tc, Output: out, IsError: isErr})
	return out, isErr
}

func (s *Session) executeTool(ctx context.Context, tc *message.ToolCall, args json.RawMessage) (message.Parts, bool) {
	if tc.Name == askUserToolName {
		// Special-cased ahead of the generic tools map: ask_user needs the
		// call's own CallID (see runAskUser and docs/design/question-tool.md
		// §2), which the generic Tool.Run signature does not carry.
		return s.runAskUser(tc.CallID, args), false
	}
	if t, ok := s.tools[tc.Name]; ok {
		out, err := t.Run(ctx, s, args)
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return out, false
	}
	if s.cfg.MCP != nil && isMCPToolName(tc.Name) {
		out, isErr, err := s.cfg.MCP.CallTool(ctx, tc.Name, args)
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return out, isErr
	}
	if s.cfg.Hooks != nil {
		resp, err := s.cfg.Hooks.ExecuteTool(ctx, &plugin.ToolExecuteRequest{
			SessionID: s.ID, CallID: tc.CallID, Tool: tc.Name, Args: args,
		})
		if err != nil {
			return message.Parts{&message.Text{Text: err.Error()}}, true
		}
		return resp.Output, resp.IsError
	}
	return message.Parts{&message.Text{Text: fmt.Sprintf("unknown tool %q", tc.Name)}}, true
}

// MCPCall routes a plugin-initiated client/mcp.call request (explicit
// server + tool name, unnamespaced) through this session's configured MCP
// registry — the exact same connected clients a namespaced mcp__<server>__
// <tool> tool call would use (see executeTool). Returns an error when no
// MCP registry is configured at all; a configured-but-unreachable server
// surfaces as an ordinary error from the registry's CallServerTool.
func (s *Session) MCPCall(ctx context.Context, server, tool string, args json.RawMessage) (message.Parts, bool, error) {
	if s.cfg.MCP == nil {
		return nil, false, fmt.Errorf("engine: no MCP servers configured")
	}
	return s.cfg.MCP.CallServerTool(ctx, server, tool, args)
}

// shellEnv collects env additions from the shell.env hook chain.
func (s *Session) shellEnv(ctx context.Context, tool, command string) map[string]string {
	if s.cfg.Hooks == nil {
		return nil
	}
	return s.cfg.Hooks.ShellEnv(ctx, &plugin.ShellEnvRequest{
		SessionID: s.ID, Tool: tool, Command: command, Dir: s.cfg.WorkDir,
	})
}
