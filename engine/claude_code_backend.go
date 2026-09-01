// The Claude Code CLI delegated-turn backend.
//
// # Why this exists
//
// A box running on a user's Claude SUBSCRIPTION (rather than metered API
// access) must run through Anthropic's own client — Anthropic blocks
// third-party harnesses from authenticating against a subscription
// directly. harness cannot BE that client for a subscription-backed turn.
// It can, however, stay the session/journal/HTTP-API front door and
// DELEGATE the turn to the real `claude` CLI, driven headlessly over its
// documented `--input-format stream-json --output-format stream-json`
// protocol, bridging that event stream into this package's own
// Session.append/emit/journal pipeline — so the console and every other
// read-path consumer see an ordinary transcript, unaware that a particular
// turn's tool loop ran inside `claude` rather than inside this package's
// own runToolCalls.
//
// # The seam
//
// A session is "delegated" purely by its model ref: message.ModelRef{
// Provider: ClaudeCodeProviderFamily, Model: "sonnet"} (or "opus", "haiku",
// any name the `claude` binary's own --model flag accepts). Two entry
// points check claudeCodeDelegated() and dispatch here, BEFORE any
// native-loop-only step runs (see each call site's own doc comment for why
// both exist rather than one):
//
//   - PromptWithOrigin (engine.go), for every ordinary Prompt/
//     PromptEngineResume call — dispatches before ContextWindowErr,
//     ensureInstructions, ensureSkills, and maybeAutoCompact, none of
//     which apply to a turn Claude Code drives with its own context
//     management.
//   - runAgenticLoop (engine.go), for goal.go's promptTurnWithRetry
//     directive-reuse retry, which calls runAgenticLoop DIRECTLY,
//     bypassing PromptWithOrigin entirely.
//
// Both roads converge on runClaudeCodeTurn below, which reads the tail of
// s.History() — always exactly one appended, not-yet-answered RoleUser
// message, whichever caller appended it — and drives ONE `claude` turn
// against it.
//
// # Event mapping
//
// Each line of the CLI's stream-json stdout is one JSON object with a
// discriminating "type" field (claudeCodeEnvelope below). This mapping is
// the best-faithful reading of the CLI's own documented stream-json
// protocol; any shape this file has not verified live against a real
// `claude` binary is called out in the relevant type/function's own
// comment, and decoding is deliberately permissive (unknown fields
// ignored, an unrecognized top-level "type" or "subtype" treated as
// inert activity rather than a hard failure) so a future CLI version
// that adds a field or event this file doesn't yet know about degrades to
// "nothing observable happened" instead of crashing a turn.
//
//   - "system"/"init": captures Claude Code's OWN session id
//     (claudeCodeCLISessionID) for --resume on this harness session's next
//     delegated turn. Any other subtype (e.g. "api_retry") is activity
//     only — observed, never fatal.
//   - "assistant": one COMPLETE API-level assistant message (text, thinking,
//     and/or tool_use content blocks together) — NOT a token-by-token
//     delta; this driver does not pass --include-partial-messages, so
//     there is nothing more granular to forward. Decoded into one
//     message.Message (Reasoning, Text, and ToolCall parts, in order),
//     appended via plain Session.append (no usage — see the usage-mapping
//     note below) and emitted as EventMessage, with one EventReasoningDelta
//     per non-empty thinking part, one EventTextDelta per non-empty text
//     part (folding the whole block's text into the message in a single
//     "delta", the closest honest match to the native
//     EventTextDelta/EventReasoningDelta contract given the CLI hands over
//     complete blocks), and one EventToolStart per tool_use part. The
//     envelope's own parent_tool_use_id (null at top level, the spawning
//     tool_use id inside a subagent's own turn) rides onto the appended
//     message as Message.ParentToolUseID, unmodified.
//   - "user": Claude Code's own tool execution results, arriving in the
//     Anthropic API's own convention of a "user"-role message carrying
//     tool_result content blocks (Claude Code executes its OWN tools here
//     — this package never calls runToolCalls for a delegated turn).
//     Decoded into one RoleTool message.Message (one ToolResult part per
//     block, Message.ParentToolUseID set the same way as the "assistant"
//     case above), appended and emitted as EventMessage, with one
//     EventToolEnd per part.
//   - "result": the turn's terminal event. consumeClaudeCodeStream RETURNS
//     as soon as this event is handled — it does not keep scanning for
//     stdout EOF (see that function's own doc comment for why this
//     matters). Never itself appended as a message (the assistant text it
//     summarizes was already appended by the last "assistant" event
//     above) — it instead carries the turn's AGGREGATE usage, applied once
//     via applyClaudeCodeUsage (a durable, message-independent record —
//     see recClaudeCodeUsage in store.go), and this turn's timing, emitted
//     once via emitTurnMetrics (ttft_ms/duration_ms, permissively zero-
//     valued if the CLI's own build does not send them). An IsError result
//     becomes this call's returned error — wrapped provider.RetryableError
//     for a known-transient shape (see claudeCodeRetryableClass), a plain
//     error otherwise. TotalCostUSD has no home in provider.Usage (no
//     adapter carries a cost field — every consumer derives cost from
//     token counts) and is deliberately dropped, not persisted.
//   - "rate_limit_event": the CLI's own subscription rate-limit/quota
//     signal, typically the SECOND event of a turn (right after
//     "system"/"init"). Never appended as a message — it carries no
//     conversational content — but mapped via mapClaudeCodeRateLimit and
//     applied to the session via applySubscriptionUsage (engine.go),
//     process-local only, surfaced on GET /session as
//     subscription_usage. See mapClaudeCodeRateLimit's own doc comment
//     for the field-by-field mapping.
//
// # Formerly deferred, now closed
//
// The v1 doc above (see git history for its original wording) flagged five
// gaps this package now closes:
//
//   - MCP passthrough (`--mcp-config`): runClaudeCodeTurn now translates
//     the session's configured MCP servers (via the mcpServerLister seam
//     below) into the CLI's own --mcp-config JSON and passes
//     --strict-mcp-config alongside it — see buildClaudeCodeMCPConfig.
//   - `--effort`: runClaudeCodeTurn reads s.Effort() and forwards it as
//     --effort, mapped through claudeCodeEffortArg.
//   - "thinking" content blocks: claudeCodeAssistantMessage now decodes
//     them into message.Reasoning parts instead of dropping them.
//   - parent_tool_use_id: captured on the envelope and carried onto the
//     appended message.Message (Message.ParentToolUseID) so a subagent
//     turn's nesting survives into the journal. The CLI only sets this
//     field on subagent assistant/user frames when told to with
//     --forward-subagent-text (default off); runClaudeCodeTurn now
//     always passes that flag alongside --verbose, so this mapping has
//     real subagent frames to read.
//   - turn_metrics: consumeClaudeCodeStream now emits an OnTurnMetrics
//     record from the "result" event's own timing/usage fields.
//
// `--append-system-prompt` remains NOT auto-populated from s.cfg.System —
// see the original reasoning, unchanged: harness's system-prompt assembly
// is deliberately native-loop-only (PromptWithOrigin's dispatch comment),
// Claude Code already discovers its own CLAUDE.md/AGENTS.md in the box
// workspace, and re-injecting harness's own native-tool-shaped instructions
// into a CLI with different tools would be actively misleading.
//
// AppendSystemPrompt is forwarded because it contains environment facts, not
// native tool instructions. The engine sends one joined option and rejects
// conflicting append-prompt options in ExtraArgs.
//
// Full goal-loop support is now PARTIAL rather than absent: a delegated
// turn's error is wrapped provider.RetryableError (see
// claudeCodeRetryableClass and the process-exit branch in
// runClaudeCodeTurn) for the known-transient shapes a "result" event or a
// child crash can report — a rate-limit/overload signal, the CLI's own
// "error_during_execution" catch-all, or the child process exiting without
// ever emitting a clean result at all — so goal.go's promptTurnWithRetry
// gives those the same backoff-and-retry treatment a native provider's
// weather gets. A deterministic failure (max turns reached, a genuine
// refusal) still surfaces as a plain error, exactly as before, so the goal
// loop still fails fast on those rather than burning a retry budget on a
// request that will fail identically every time.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// ClaudeCodeProviderFamily is the message.ModelRef.Provider value that
// selects this delegated-turn backend — see claudeCodeDelegated. It
// matches config.TypeClaudeCodeCLI's conventional providers-map key and
// provider/claudecode.Family (that package cannot import this one — see
// its own doc comment for why the string is duplicated there rather than
// imported, and TestClaudeCodeProviderFamilyMatchesClaudecodePackage for
// the parity check).
const ClaudeCodeProviderFamily = "claude-code"

// defaultClaudeCodeBinaryPath is ClaudeCodeConfig.BinaryPath's zero-value
// default (newSession) — the CLI's own published binary name, resolved via
// PATH like any exec.
const defaultClaudeCodeBinaryPath = "claude"

// claudeCodeInterruptGrace bounds how long runClaudeCodeTurn waits for the
// `claude` child to exit on its own after each escalating signal, before
// sending the next one — see runClaudeCodeTurn's signal-cascade goroutine.
// A var, not a const, so a test can shrink it rather than paying the real
// wall-clock cost.
var claudeCodeInterruptGrace = 5 * time.Second

// claudeCodeStderrCap bounds how much of the `claude` child's stderr this
// file retains for an error message — enough to be a useful diagnostic
// (a missing binary, a permission error, an early crash) without letting
// a runaway or malicious child exhaust memory buffering it.
const claudeCodeStderrCap = 4096

// ClaudeCodeConfig configures the delegated-turn backend — see
// Config.ClaudeCode's own doc comment. It is engine's own minimal
// translation target for config.Provider's BinaryPath/ExtraArgs/
// PermissionMode fields (cmd/harness's claudeCodeConfigFor does the
// translation): package engine does not import package config, the same
// separation every other Config field already keeps.
type ClaudeCodeConfig struct {
	// BinaryPath is the `claude` executable to spawn, resolved via PATH
	// like any exec. Empty defaults to "claude" (newSession).
	BinaryPath string
	// ExtraArgs follow engine-owned flags. Append-prompt options conflict with
	// AppendSystemPrompt and are rejected when that field is set.
	ExtraArgs []string
	// PermissionMode, if non-empty, becomes --permission-mode <value>.
	PermissionMode string
	// HTTPBaseURL is the harness HTTP server's own loopback base URL (e.g.
	// "http://127.0.0.1:4096" — cmd/harness's serveCmd derives it from its
	// own -addr the same way it already does for the plugin host, via
	// serveURLForAddr), or "" when this session is not being served over
	// HTTP at all (e.g. a one-shot `harness run`). It is the ONLY thing
	// this package needs to reach the harness-hosted get_conversation_history
	// MCP tool at /session/{id}/mcp (server/server.go) — see
	// claudeCodeMCPConfigFile, which appends a synthetic "http" server
	// entry naming <HTTPBaseURL>/session/<s.ID>/mcp whenever this is
	// non-empty, regardless of whether Config.MCP configures any servers
	// of its own. Empty disables the synthetic entry entirely: there is no
	// endpoint for a delegated turn to call in that mode, so advertising
	// one would just be a dead tool.
	HTTPBaseURL string
	// HTTPAuthToken, when non-empty, is sent as an "Authorization: Bearer
	// <token>" header on the synthetic history-server entry above — the
	// same bearer scheme server.Server.authorized checks for every other
	// route. Empty (e.g. a loopback-only Unauthenticated serve, per
	// server.Options.Unauthenticated) omits the header entirely rather
	// than sending an empty bearer value.
	HTTPAuthToken string
}

// claudeCodeToolsServerName is the synthetic --mcp-config server name
// claudeCodeMCPConfigFile registers for the harness-hosted MCP server
// (server/mcp_history.go's POST /session/{id}/mcp — get_conversation_history
// plus, when configured, the native `process` tool) — see
// ClaudeCodeConfig.HTTPBaseURL's own doc comment. A fixed, harness-
// namespaced name (never an operator-configured Config.MCP key) so it can
// never collide with one.
const claudeCodeToolsServerName = "harness-tools"

// claudeCodeHistoryServerURL returns the synthetic history-server's own
// per-session URL — <HTTPBaseURL>/session/<s.ID>/mcp — or "" when
// ClaudeCodeConfig.HTTPBaseURL is unset (see its own doc comment for why
// that disables the entry entirely).
func (s *Session) claudeCodeHistoryServerURL() string {
	base := strings.TrimRight(s.cfg.ClaudeCode.HTTPBaseURL, "/")
	if base == "" {
		return ""
	}
	return base + "/session/" + s.ID + "/mcp"
}

// claudeCodeDelegated reports whether s's CURRENT model routes to this
// delegated backend rather than a native provider.Provider — see
// ClaudeCodeProviderFamily.
func (s *Session) claudeCodeDelegated() bool {
	return s.Model().Provider == ClaudeCodeProviderFamily
}

// claudeCodeSessionID returns the Claude Code CLI's own session id
// captured on this session's most recent delegated turn (see
// Session.claudeCodeCLISessionID's own doc comment), or "" before the
// first one has completed an init event.
func (s *Session) claudeCodeSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claudeCodeCLISessionID
}

// recordClaudeCodeSessionID durably records id as this session's Claude
// Code CLI session id, for --resume on every later delegated turn. A no-op
// when id is empty or already recorded, so a repeat init event (there is
// exactly one per turn, but nothing prevents a future CLI version from
// emitting more) never writes a redundant journal record.
func (s *Session) recordClaudeCodeSessionID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claudeCodeCLISessionID == id {
		return
	}
	s.claudeCodeCLISessionID = id
	s.persistClaudeCodeSessionID(id)
}

// claudeCodeHistoryWatermarkCount returns Session.claudeCodeHistoryWatermark
// — see its own doc comment.
func (s *Session) claudeCodeHistoryWatermarkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claudeCodeHistoryWatermark
}

// recordClaudeCodeHistoryWatermark durably records n as this session's
// claudeCodeHistoryWatermark — see that field's own doc comment. A no-op
// when n already matches the recorded value, so a turn that leaves
// s.History()'s length unchanged never writes a redundant journal record.
func (s *Session) recordClaudeCodeHistoryWatermark(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claudeCodeHistoryWatermark == n {
		return
	}
	s.claudeCodeHistoryWatermark = n
	s.persistClaudeCodeHistoryWatermark(n)
}

// applyClaudeCodeUsage folds a delegated turn's AGGREGATE usage (the
// "result" event's own usage field, covering every internal API call
// Claude Code made across the whole turn — not just the closing one) into
// Session.Usage()/LastUsage(), durably (recClaudeCodeUsage, store.go).
//
// This is deliberately NOT routed through appendWithUsage: by the time a
// "result" event arrives, every message this turn produced has already
// been appended (plain Session.append, no usage attached — see this
// file's package doc, "final result" bullet) in receipt order, matching
// each one to the EventMessage/EventToolStart/EventToolEnd emit it needs
// at the moment it actually happened; retroactively re-appending a
// duplicate "terminal" message purely to give appendWithUsage somewhere to
// attach Usage would double the journal's message count for every
// delegated turn. Unlike compact.go's recCompact precedent (which folds
// its own Usage into cumulative ONLY, deliberately leaving lastUsage
// alone, because compact's caller must keep sizing auto-compaction off the
// last REAL prompt), this DOES set lastUsage: harness's own auto-
// compaction never runs for a delegated session (see PromptWithOrigin's
// dispatch), so there is no native trigger signal here to protect, and
// GET /session should still report an accurate last-turn size.
func (s *Session) applyClaudeCodeUsage(usage provider.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.InputTokens += usage.InputTokens
	s.usage.OutputTokens += usage.OutputTokens
	s.usage.CacheReadTokens += usage.CacheReadTokens
	s.usage.CacheWriteTokens += usage.CacheWriteTokens
	s.lastUsage = usage
	s.haveLastUsage = true
	s.persistClaudeCodeUsage(usage)
}

// runClaudeCodeTurn drives ONE turn through the `claude` CLI against s's
// CURRENT history tail — the single, most-recently-appended, not-yet-
// answered RoleUser message, appended by whichever of PromptWithOrigin or
// goal.go's directive-reuse retry called in (see this file's package doc).
// It returns the final assistant message.Message on success.
//
// Every message this turn produces is appended to s.History() as it is
// decoded from the child's stdout, so a turn that fails PARTWAY THROUGH —
// after some tool calls already ran — still leaves an accurate, ungapped
// transcript behind; only the (*message.Message, error) return itself
// reports the failure, exactly like the native path's
// interruptedTurnError partial-append behavior (engine.go).
func (s *Session) runClaudeCodeTurn(ctx context.Context) (*message.Message, error) {
	history := s.History()
	text := lastUserMessageText(history)
	if text == "" {
		return nil, errors.New("engine: claude-code delegated turn found no pending user message to answer")
	}
	// Deliver any pending task notifications (a settled child's Done/Failed
	// result) into THIS turn's input, mirroring the native loop's own
	// checkout step (engine.go's streamTurn, via withAmbientStatus) — see
	// checkoutTaskNotificationsSegment's doc comment for the checkout/
	// commit/requeue two-phase handoff this call is one leg of; the other
	// two legs (commit on success, requeue on failure) live one layer up,
	// in runAgenticLoop's claudeCodeDelegated branch, exactly like the
	// native path's own commit/requeue calls.
	//
	// There is no EngineContext wire concept for the stream-json CLI
	// stdin protocol this file drives (unlike a native provider request,
	// which carries the segment as its own trust-tagged part — see
	// withAmbientStatus), so the rendered segment is spliced directly into
	// the plain text the CLI receives instead, on its own blank-line-
	// separated block. Deliberately NOT wrapped in RenderEngineContext's
	// <harness-engine-context> sentinel: that sentinel's meaning is taught
	// by the NATIVE base system prompt's ambientContextGuidance
	// (cmd/harness/main.go), which a delegated turn never sends — Claude
	// Code drives this turn under its own, unrelated system prompt (see
	// this file's package doc, "`--append-system-prompt` remains NOT
	// auto-populated from s.cfg.System"), so the tag would reach the model
	// as meaningless literal text rather than a recognized trust marker.
	// The rendered segment is still self-delimited (renderTaskNotifications'
	// own "[tasks:\n- ...\n]" shape) and still defended the same way a
	// native request's block is: renderTaskNotifications already runs
	// every free-text field (a child's own untrusted Result, or a
	// provider's FailReason) through neutralizeNotificationText, which
	// strips newlines so a child cannot manufacture a fake sibling "- ..."
	// entry that reads as a second, forged notification — that protection
	// is applied before this splice and does not depend on the sentinel.
	// A no-op (text unchanged) when nothing is pending, matching
	// withAmbientStatus's own no-op-on-empty-segment behavior.
	if seg := s.checkoutTaskNotificationsSegment(); seg != "" {
		text += "\n\n" + seg
	}

	cfg := s.cfg.ClaudeCode
	binary := cfg.BinaryPath
	if binary == "" {
		binary = defaultClaudeCodeBinaryPath
	}
	model := s.Model()

	appendPrompt, haveAppendPrompt := claudeCodeAppendSystemPrompt(s.cfg.AppendSystemPrompt)
	if haveAppendPrompt {
		for _, arg := range cfg.ExtraArgs {
			if claudeCodeAppendPromptArg(arg) {
				return nil, fmt.Errorf("engine: claude-code: Config.ClaudeCode.ExtraArgs contains %q, which conflicts with Config.AppendSystemPrompt; remove the extra arg and put the text in AppendSystemPrompt", arg)
			}
		}
	}

	// The --mcp-config file (if s.cfg.MCP has any servers to describe) is
	// written before the args slice below so its path can be included, and
	// removed unconditionally on every return path via cleanupMCPConfig —
	// see claudeCodeMCPConfigFile's own doc comment for why this rides a
	// temp file rather than an inline argv value.
	mcpConfigPath, cleanupMCPConfig, err := s.claudeCodeMCPConfigFile()
	if err != nil {
		return nil, err
	}
	defer cleanupMCPConfig()

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// The CLI defaults subagent (Task) assistant/user frames to a flat,
		// unparented stream unless told otherwise. Without this flag, a
		// subagent's activity carries no parent_tool_use_id, so a consumer
		// like the boxes console can't nest it under its spawning Task and
		// renders it inline instead. --forward-subagent-text makes the CLI
		// set parent_tool_use_id on those frames, which this driver already
		// reads onto Message.ParentToolUseID (see the package doc above).
		// It is gated only on --print/--output-format=stream-json, both of
		// which this driver always sets, so it is safe to pass
		// unconditionally.
		"--forward-subagent-text",
		// All subagent spawning in the claude-code lane must go through
		// harness's own cross-family "task" tool (server/mcp_history.go,
		// #223), never the CLI's native same-family equivalent — the two
		// tools below are the CLI's only built-in ways to spawn a
		// same-family subagent (Agent: single subagent/teammate; Workflow:
		// a script that fans out many subagents), so both are disallowed
		// unconditionally rather than left for the model to choose between.
		"--disallowedTools", "Agent,Workflow",
	}
	if model.Model != "" {
		args = append(args, "--model", model.Model)
	}
	if resumeID := s.claudeCodeSessionID(); resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	// See claudeCodeHistoryDirectiveArgs's own doc comment: nil (a no-op
	// append) unless history holds conversation the CLI's own resumed
	// session (if any) has not already incorporated — deliberately
	// independent of resumeID above, since a model switch away from
	// claude-code and back leaves the CLI session id in place but can
	// still leave it stale relative to history.
	args = append(args, claudeCodeHistoryDirectiveArgs(history, s.claudeCodeHistoryWatermarkCount())...)
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	if effort, ok := claudeCodeEffortArg(s.Effort()); ok {
		args = append(args, "--effort", effort)
	}
	if haveAppendPrompt {
		args = append(args, "--append-system-prompt", appendPrompt)
	}
	if mcpConfigPath != "" {
		// --strict-mcp-config: only harness's own configured servers are
		// visible to the child, never whatever a project-local .mcp.json or
		// the operator's own ~/.claude.json might additionally define —
		// harness's config is the single source of truth for what tools a
		// delegated turn can reach, exactly like the native loop's own
		// toolDefs assembly.
		args = append(args, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
	}
	args = append(args, cfg.ExtraArgs...)

	cmd := exec.Command(binary, args...) //nolint:gosec // binary/args are operator config, not request input
	cmd.Dir = s.cfg.WorkDir
	// Auth is deliberately NOT this package's job: no ANTHROPIC_API_KEY is
	// set, no ~/.claude/.credentials.json is read or written here. The
	// child inherits harness's own ambient environment verbatim — on the
	// boxes platform, that is whatever placeholder credential material and
	// gatekeeper routing the BOX already set up before harness ever ran
	// (see this file's package doc). cmd.Env left nil means exactly that:
	// os/exec's own documented behavior is to inherit os.Environ().

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("engine: claude-code: creating stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("engine: claude-code: creating stdout pipe: %w", err)
	}
	// stderr is deliberately read via cmd.StderrPipe(), NOT handed to Cmd
	// as a plain cmd.Stderr = io.Writer, even though the latter reads
	// simpler. Assigning a non-*os.File io.Writer makes Cmd allocate its
	// OWN internal pipe and copying goroutine (os/exec's writerDescriptor)
	// and register that goroutine so cmd.Wait() BLOCKS on it via
	// awaitGoroutines — i.e. Wait() would not return until stderr's pipe
	// sees EOF, which reintroduces exactly the hazard this file's fix to
	// consumeClaudeCodeStream just removed from stdout: a `claude --bg`
	// turn's leaked descendant (a dev server, say) commonly inherits BOTH
	// fd 1 AND fd 2, so it can wedge Wait() through stderr even once
	// stdout no longer can. StderrPipe's read end instead lands in
	// Cmd.parentIOPipes, which Wait() only CLOSES, never waits on (see
	// this call's own comment on cmd.Wait(), below) — so this file must
	// drain it itself, in the anonymous goroutine started right after
	// Start() below.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("engine: claude-code: creating stderr pipe: %w", err)
	}
	var stderr capBuffer
	stderr.cap = claudeCodeStderrCap

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("engine: claude-code: starting %q: %w", binary, err)
	}

	// Drain stderrPipe into stderr for as long as it stays open, same as
	// Cmd's own now-avoided internal copying goroutine would have —
	// capturing a real crash's full stderr text (bounded by
	// claudeCodeStderrCap) for the ordinary case where the direct child is
	// the pipe's only writer and closes it on exit. This goroutine is
	// deliberately NEVER joined by the rest of this call (see cmd.Wait()'s
	// own comment on why that is safe rather than a leak): io.Copy's
	// blocked Read ends on its own, promptly, the moment cmd.Wait() closes
	// stderrPipe's read end below — whether that close finds EOF already
	// pending (the ordinary case) or a leaked descendant still holding the
	// write end open (the `--bg` case this file's package doc describes).
	go func() {
		_, _ = io.Copy(&stderr, stderrPipe)
	}()

	inputLine, err := json.Marshal(claudeCodeInputMessage{
		Type: "user",
		Message: claudeCodeInputInnerMessage{
			Role:    "user",
			Content: text,
		},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("engine: claude-code: encoding turn input: %w", err)
	}
	// inputErr captures a failure writing or closing stdin WITHOUT killing
	// the child or returning early: a fast/trivial turn's child can
	// legitimately finish its whole result and exit (closing its own end
	// of the pipe) before this call finishes writing/closing its side,
	// which turns an otherwise-harmless race into a broken-pipe/closed-
	// pipe error right here. That is not a real failure — the child still
	// has a complete, valid result waiting on stdout — so this call must
	// keep going and read it: only if the turn ends with NO usable result
	// at all does inputErr get promoted to the actual returned error,
	// below. Deliberately not stdin.Close() after a failed Write: closing
	// an already-broken pipe has nothing useful to report, and calling it
	// anyway would risk overwriting a meaningful inputErr with a second,
	// less informative one.
	var inputErr error
	if _, err := stdin.Write(append(inputLine, '\n')); err != nil {
		inputErr = fmt.Errorf("engine: claude-code: writing turn input: %w", err)
	} else if err := stdin.Close(); err != nil {
		// Close stdin: this driver sends exactly one turn per `claude`
		// child (continuity across harness turns is --resume, not a long-
		// lived child — see this file's package doc), and an unclosed
		// stdin would leave the CLI waiting indefinitely for a second
		// message that is never coming, wedging cmd.Wait() below forever —
		// but a Close failing is itself just as benign as a Write failing,
		// for the exact same reason (the read end may already be gone
		// because the child already finished), so it gets the same
		// deferred treatment as the Write error above rather than an
		// immediate kill-and-return.
		inputErr = fmt.Errorf("engine: claude-code: closing stdin: %w", err)
	}

	// The signal-abort cascade: SIGINT first (Claude Code's own docs
	// describe this as ending the current turn gracefully, leaving it
	// --resume-able), escalating to SIGTERM and finally an unconditional
	// Kill if the child does not exit within claudeCodeInterruptGrace of
	// each — so a harness-side abort/shutdown always eventually reaps a
	// wedged child rather than leaking it. done is closed once this
	// call's own Wait returns, so the goroutine never fires a signal at a
	// process this call has already reaped.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		proc := cmd.Process
		if proc == nil {
			return
		}
		_ = proc.Signal(syscall.SIGINT)
		select {
		case <-done:
			return
		case <-time.After(claudeCodeInterruptGrace):
		}
		_ = proc.Signal(syscall.SIGTERM)
		select {
		case <-done:
			return
		case <-time.After(claudeCodeInterruptGrace):
		}
		_ = proc.Kill()
	}()

	finalMsg, started, turnErr := s.consumeClaudeCodeStream(stdout, model)
	if started {
		// The CLI's own session actually came up for this turn — whether
		// or not turnErr is set (see claudeCodeTurnResult's own o.started
		// branch for the identical "started but errored is still real
		// activity" reasoning) — so by now it has incorporated everything
		// currently in s.History(): either it produced those messages
		// itself this turn, or an earlier turn's get_conversation_history
		// pull already covered the rest. Recording that here, not only on
		// a successful result, is what lets claudeCodeHistoryDirectiveArgs
		// tell a genuinely stale resumed session (history grew via an
		// intervening native-provider turn) apart from one that is merely
		// mid-turn.
		s.recordClaudeCodeHistoryWatermark(len(s.History()))
	}

	// cmd.Wait() below does NOT reintroduce the EOF wait that
	// consumeClaudeCodeStream's early return on "result" just avoided, on
	// EITHER stdout or stderr, and it does not touch (let alone kill) a
	// surviving `claude --bg` daemon:
	//
	//   - Wait() waits on c.Process.Wait(), i.e. waitpid(2) on the DIRECT
	//     `claude` child's own PID. That is a wait for one specific
	//     process's exit status, never a wait for a pipe's write end to
	//     see every holder close it. The direct child has already printed
	//     its "result" and exited by the time consumeClaudeCodeStream
	//     returns, so this waitpid returns immediately.
	//   - Both stdout and stderr here came from Cmd's own *Pipe methods
	//     (cmd.StdoutPipe() above, cmd.StderrPipe() further above) rather
	//     than a plain cmd.Stdout/cmd.Stderr io.Writer. That distinction is
	//     load-bearing: os/exec's writerDescriptor allocates an internal
	//     pipe AND a copying goroutine ONLY for a plain io.Writer target,
	//     and registers that goroutine in Cmd.goroutineErr, which Wait()'s
	//     own awaitGoroutines step explicitly BLOCKS on until it finishes
	//     (i.e. until that pipe sees EOF). This file used to hand stderr to
	//     Cmd exactly that way (cmd.Stderr = &capBuffer), which — even
	//     after stdout's own fix above — still let a leaked `--bg`
	//     descendant that inherits fd 2 (a dev server commonly inherits
	//     BOTH fd 1 and fd 2, not just fd 1) wedge Wait() through stderr
	//     alone. Both pipes are now read by THIS file's own code instead
	//     (consumeClaudeCodeStream for stdout, the anonymous drain
	//     goroutine started right after cmd.Start() above for stderr), so
	//     Cmd.goroutineErr stays nil and awaitGoroutines has nothing to
	//     block on for either fd.
	//   - Go's os/exec source (os/exec/exec.go, StdoutPipe/StderrPipe)
	//     records each pipe's READ end (the `stdout` and `stderrPipe`
	//     variables this call reads) in Cmd.parentIOPipes, and Wait()
	//     closes every entry of parentIOPipes itself right after the
	//     waitpid above returns. So Wait() actively closes both of our
	//     read ends for us; it never blocks on either.
	//   - The leaked grandchild only ever held the pipes' WRITE ends (fds
	//     duplicated across fork/exec). Wait() closing our read ends does
	//     not signal or touch that process at all — the background daemon
	//     keeps running untouched, exactly as `claude --bg` intends. A
	//     later write of its own may see EPIPE/SIGPIPE once nothing is
	//     left to read that fd, which is the same fate any well-behaved
	//     detached daemon should already tolerate by redirecting its own
	//     stdio, not a signal this call sends it.
	waitErr := cmd.Wait()

	return claudeCodeTurnResult(claudeCodeTurnOutcome{
		ctxErr:   ctx.Err(),
		turnErr:  turnErr,
		waitErr:  waitErr,
		inputErr: inputErr,
		started:  started,
		finalMsg: finalMsg,
		binary:   binary,
		stderr:   stderr.String(),
	})
}

// claudeCodeTurnOutcome collects everything runClaudeCodeTurn learns about
// one delegated turn's process/stream lifecycle — see claudeCodeTurnResult,
// the sole consumer, for what each field decides.
type claudeCodeTurnOutcome struct {
	ctxErr   error
	turnErr  error
	waitErr  error
	inputErr error
	started  bool
	finalMsg *message.Message
	binary   string
	stderr   string
}

// claudeCodeTurnResult turns one claudeCodeTurnOutcome into runClaudeCodeTurn's
// own (*message.Message, error) return. Split out from runClaudeCodeTurn as
// its own pure function so the precedence between a caller abort, a
// classified result error, a process-exit error, and a benign input-write
// race is unit-testable directly (TestClaudeCodeTurnResult) without having
// to force each interleaving out of a real child process.
func claudeCodeTurnResult(o claudeCodeTurnOutcome) (*message.Message, error) {
	if o.ctxErr != nil {
		// An abort/shutdown-driven cancellation always wins over whatever
		// the stream decoded — the same precedence the native path's
		// context.Canceled handling gives ctx (engine.go's
		// streamTurnWithRetry/runAgenticLoop treat a canceled context as a
		// deliberate stop, not an ordinary failure).
		return nil, o.ctxErr
	}
	if o.turnErr != nil {
		return nil, o.turnErr
	}
	if o.waitErr != nil {
		msg := fmt.Sprintf("engine: claude-code: %q exited with error: %v", o.binary, o.waitErr)
		if o.stderr != "" {
			msg += fmt.Sprintf(" (stderr: %s)", o.stderr)
		}
		err := error(errors.New(msg))
		if o.started {
			// The child got far enough to emit at least one "system" event
			// — the CLI's own protocol came up, so a session genuinely
			// started — and then exited without ever emitting a clean
			// "result" event (o.turnErr == nil, so this is not the
			// deterministic IsError shape claudeCodeRetryableClass
			// classifies above): a crash, an OOM kill, a signal from
			// something other than this call's own abort cascade (o.ctxErr
			// was already checked nil above). That is exactly the same
			// kind of non-deterministic, worth-a-retry provider weather
			// MarkStreamTruncated marks for a native adapter's stream that
			// dies mid-body, so goal.go's promptTurnWithRetry gives it the
			// same backoff-and-retry treatment rather than parking on
			// attempt 1.
			err = provider.MarkRetryable(err, provider.RetryableServerError)
		}
		// !o.started means the child never even got its own protocol off
		// the ground — an unknown flag on an older `claude` build, a
		// malformed --mcp-config command, an invalid --model value, a
		// missing binary's exec succeeding but the binary itself refusing
		// to run — deterministic startup failures that will fail
		// identically on every retry. Marking THOSE retryable would have a
		// PursueGoal loop burn its entire retryable budget
		// (goalRetryableMaxAttempts, goal.go) with backoff before parking,
		// delaying the surfacing of what is really a config error nothing
		// will fix by waiting. Left a plain error here, exactly like every
		// other deterministic failure this file returns.
		return nil, err
	}
	if o.finalMsg == nil {
		if o.inputErr != nil {
			// No usable result at all, AND writing/closing stdin itself
			// failed: the write error is almost certainly the actual root
			// cause here (the child never got the turn's own prompt to
			// answer), so surface it in place of the generic "no assistant
			// message" error below.
			return nil, o.inputErr
		}
		return nil, errors.New("engine: claude-code: turn ended with no assistant message")
	}
	// o.finalMsg != nil: the child produced a complete, usable result
	// despite any o.inputErr recorded above (see runClaudeCodeTurn's own
	// inputErr doc comment). A benign broken-pipe/closed-pipe race
	// resolves itself once the turn's own output proves the child got
	// everything it needed; o.inputErr is deliberately dropped here, never
	// surfaced once the turn otherwise succeeded.
	return o.finalMsg, nil
}

// lastUserMessageText returns the Text of the LAST message in history if
// it is a RoleUser message, or "" otherwise. runClaudeCodeTurn's one
// caller-contract requirement is that its caller has already appended
// (or, for the goal-loop directive-reuse retry, left in place) exactly one
// unanswered RoleUser message at the tail — this reads it back rather than
// threading the text through an extra parameter, so both call sites (a
// fresh append, and a retry that appends nothing new) share one path.
func lastUserMessageText(history []message.Message) string {
	if len(history) == 0 {
		return ""
	}
	last := history[len(history)-1]
	if last.Role != message.RoleUser {
		return ""
	}
	return last.Parts.Text()
}

// claudeCodeHistoryDirective is the --append-system-prompt text
// runClaudeCodeTurn appends exactly once per delegated session — see
// claudeCodeHistoryDirectiveArgs. It tells the CLI to call the
// harness-hosted get_conversation_history tool (server/mcp_history.go's
// POST /session/{id}/mcp, advertised via the claudeCodeToolsServerName
// entry claudeCodeMCPConfigFile writes) before answering, so a session that
// switches to claude-code mid-conversation (or on its first-ever
// claude-code turn) does not start blind to everything that already
// happened: stream-json INPUT cannot seed prior history (a "user" line
// gets live re-executed; an "assistant" line is dropped or crashes the
// CLI — see this file's package doc for why this pull-based tool exists
// instead), so this is the one nudge that gets the CLI to pull it itself.
const claudeCodeHistoryDirective = "You are continuing a conversation that happened on another model. Before responding, call the get_conversation_history tool to read what happened so far."

// claudeCodeHistoryDirectiveArgs returns the --append-system-prompt argv
// pair (flag plus value) whenever history holds conversation the CLI's own
// resumed session (if any) has NOT already incorporated — priorCount >
// watermark, where priorCount is len(history) minus the single pending
// trigger message runClaudeCodeTurn is about to answer (lastUserMessageText's
// own caller contract: history's last element is always that pending
// message) and watermark is Session.claudeCodeHistoryWatermarkCount(), the
// message count as of the end of whichever delegated turn last ran. nil
// (priorCount <= watermark) in two cases:
//
//   - A session's genuine first-ever message: watermark is 0 (no delegated
//     turn has ever run) and priorCount is also 0 (no prior history at
//     all). Calling get_conversation_history would return nothing useful.
//   - Consecutive claude-code turns with nothing new in between: the
//     previous turn's own watermark update already accounts for
//     everything currently in history, including that turn's own answer.
//     The CLI's --resume'd session already carries forward whichever
//     earlier turn's own get_conversation_history tool_result — re-pulling
//     here would waste a call the CLI has no reason to repeat (see this
//     file's package doc, "one call suffices").
//
// This is deliberately NOT gated on Session.claudeCodeSessionID() (whether a
// CLI session id is recorded at all): a model switch away from claude-code
// and back leaves claudeCodeCLISessionID untouched (see its own doc
// comment), so a resumed CLI session can still be stale relative to
// s.history even though resumeID != "" — exactly the case an intervening
// native-provider turn (or several) produces. watermark, not resumeID's
// emptiness, is what actually answers "has the CLI's session seen
// everything currently in history".
func claudeCodeHistoryDirectiveArgs(history []message.Message, watermark int) []string {
	priorCount := len(history) - 1
	if priorCount < 0 {
		priorCount = 0
	}
	if priorCount <= watermark {
		return nil
	}
	return []string{"--append-system-prompt", claudeCodeHistoryDirective}
}

// consumeClaudeCodeStream reads newline-delimited stream-json events from r
// (the `claude` child's stdout), appending/emitting each decoded event per
// this file's package-doc mapping, and returns the last assistant
// message.Message it appended (nil if none), whether the child got far
// enough to emit at least one "system" event (see the started return
// value's own use in runClaudeCodeTurn's waitErr branch: it is what tells a
// child that never even started apart from one that started and later
// crashed), and any turn-ending error a "result" event's IsError reported.
//
// It returns as soon as it handles the "result" event rather than reading
// on to stdout's EOF, because "result" IS the turn's documented terminal
// event (see this file's package doc) and EOF is not a safe thing to wait
// for: EOF on a pipe only arrives once EVERY process holding the write end
// open has exited, and the direct `claude` child is not the only process
// that can hold it. `claude --bg` — the CLI's own sanctioned pattern for a
// long-running background task — spawns a daemon that reparents to PID 1
// and is meant to keep running after the turn ends; that daemon (or a
// further child of its own, e.g. a dev server it starts) inherits this
// process's file descriptors, including stdout, unless it explicitly
// closes or redirects them. The direct `claude` child still prints its own
// "result" and exits right on schedule, but the leaked descendant keeps
// the pipe's write end open indefinitely, so a scan loop that waits for
// EOF blocks forever — wedging the harness turn (and the whole session)
// while the surviving daemon keeps running exactly as designed. Returning
// on "result" decouples turn completion from every descendant's fd
// lifetime, which is also why this must never be "solved" by killing the
// child's process group: that would kill the very background session
// --bg exists to keep alive. See runClaudeCodeTurn's own comment on why
// its subsequent cmd.Wait() does not reintroduce this wait.
func (s *Session) consumeClaudeCodeStream(r io.Reader, model message.ModelRef) (finalMsg *message.Message, started bool, turnErr error) {
	scanner := bufio.NewScanner(r)
	// A tool call's arguments or a large tool result can exceed
	// bufio.Scanner's 64KiB default token size; 8MiB comfortably covers
	// any realistic single stream-json line without an unbounded read.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env claudeCodeEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			// A line this decoder cannot even parse as JSON: never crash a
			// turn over one malformed/unexpected line from the child —
			// see this file's package doc on permissive decoding.
			continue
		}
		switch env.Type {
		case "system":
			// ANY "system" event — not only subtype "init" — is proof the
			// child's stream-json protocol actually came up: init is
			// documented as the first event a real `claude` binary ever
			// emits, so seeing one at all (whatever its subtype) means the
			// session started. See the started return value's own doc
			// comment above.
			started = true
			if env.Subtype == "init" {
				s.recordClaudeCodeSessionID(env.SessionID)
			}
			// Any other subtype (e.g. "api_retry") is observed but
			// requires no action — see the package doc's event-mapping
			// section.
		case "assistant":
			msg := claudeCodeAssistantMessage(env.Message, model, env.ParentToolUseID)
			if len(msg.Parts) == 0 {
				continue
			}
			s.append(msg)
			s.emit(Event{Type: EventMessage, Message: &msg})
			for _, p := range msg.Parts {
				switch part := p.(type) {
				case *message.Text:
					if part.Text != "" {
						s.emit(Event{Type: EventTextDelta, Text: part.Text})
					}
				case *message.Reasoning:
					if part.Text != "" {
						s.emit(Event{Type: EventReasoningDelta, Text: part.Text})
					}
				case *message.ToolCall:
					s.emit(Event{Type: EventToolStart, ToolCall: part})
				}
			}
			finalMsg = &msg
		case "user":
			msg := claudeCodeToolResultMessage(env.Message, env.ParentToolUseID)
			if msg == nil {
				continue
			}
			s.append(*msg)
			s.emit(Event{Type: EventMessage, Message: msg})
			for _, p := range msg.Parts {
				if tr, ok := p.(*message.ToolResult); ok {
					s.emit(Event{
						Type:     EventToolEnd,
						ToolCall: &message.ToolCall{CallID: tr.CallID},
						Output:   tr.Content,
						IsError:  tr.IsError,
					})
				}
			}
		case "result":
			usage := mapClaudeCodeUsage(env.Usage)
			s.applyClaudeCodeUsage(usage)
			streamMillis := env.DurationMillis - env.TTFTMillis
			if streamMillis < 0 {
				// A CLI build that sends duration_ms but not ttft_ms (or
				// vice versa) must never produce a negative StreamMillis —
				// see TurnMetrics.StreamMillis's own "zero when EventDone
				// was itself the first delta" precedent for the native
				// path; here it means "no usable breakdown", not "the
				// turn took negative time".
				streamMillis = 0
			}
			s.emitTurnMetrics(TurnMetrics{
				SessionID:        s.ID,
				Model:            model,
				Attempt:          1, // see this file's package doc: Claude Code retries internally, invisible to harness
				TTFTMillis:       env.TTFTMillis,
				StreamMillis:     streamMillis,
				InputTokens:      usage.InputTokens,
				OutputTokens:     usage.OutputTokens,
				CacheReadTokens:  usage.CacheReadTokens,
				CacheWriteTokens: usage.CacheWriteTokens,
			})
			if env.IsError {
				turnErr = fmt.Errorf("engine: claude-code: turn ended in error (subtype %q): %s", env.Subtype, env.Result)
				if class, ok := claudeCodeRetryableClass(env.Subtype, env.Result); ok {
					turnErr = provider.MarkRetryable(turnErr, class)
				}
			}
			// TotalCostUSD is intentionally dropped here — see the
			// package doc's "result" bullet: provider.Usage has no cost
			// field for it to occupy.
			//
			// Return NOW rather than falling through to another
			// scanner.Scan(): "result" is the documented terminal event
			// (package doc above), and continuing to scan would instead
			// wait for stdout's EOF — which a `claude --bg` turn's leaked
			// descendant fd can withhold forever. See this function's own
			// doc comment for the full reasoning. Any bytes a lingering
			// writer emits after this point are simply never read by this
			// call; they are not folded into the turn in any way.
			//
			// This is safe for "rate_limit_event" specifically, not just
			// documented that way: live-verified against a real `claude`
			// 2.1.252 binary (two separate turns, one plain and one with a
			// tool call) that "result" is the LAST line the direct child
			// ever emits — rate_limit_event, when present, arrived before
			// it both times, never after. Nothing observed contradicts the
			// package doc's "result" bullet calling it the turn's terminal
			// event.
			return finalMsg, started, turnErr
		case "rate_limit_event":
			// The CLI's own subscription rate-limit/quota signal — see
			// mapClaudeCodeRateLimit's own doc comment for the wire shape
			// and mapping. Typically the SECOND event of a turn (right
			// after "system"/"init"), but this file reacts to it whenever
			// it arrives, and to every occurrence, not only the first: a
			// long-running turn can see its own limits shift mid-turn.
			if usage, ok := mapClaudeCodeRateLimit(env.RateLimitInfo); ok {
				s.applySubscriptionUsage(usage)
			}
		}
		// Any other top-level "type" (this driver has none documented
		// beyond the four above) is inert activity, per the package doc.
	}
	return finalMsg, started, turnErr
}

// claudeCodeEnvelope is the outer discriminator every line of `claude
// --output-format stream-json` decodes into. Fields are read permissively
// (encoding/json ignores JSON fields with no matching Go field, and every
// field here is optional so a line missing one just zero-values it) —
// see this file's package doc for the decoding philosophy.
type claudeCodeEnvelope struct {
	Type      string           `json:"type"`
	Subtype   string           `json:"subtype,omitempty"`
	SessionID string           `json:"session_id,omitempty"`
	Message   json.RawMessage  `json:"message,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Result    string           `json:"result,omitempty"`
	Usage     *claudeCodeUsage `json:"usage,omitempty"`
	// TotalCostUSD is Claude Code's own dollar-cost accounting for the
	// whole delegated turn — read but deliberately never mapped onto
	// anything (see this file's package doc, "result" bullet).
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	// ParentToolUseID is null (so absent, or explicit JSON null — either
	// decodes to "" for a plain string field, encoding/json's documented
	// no-op-on-null behavior for a non-pointer target) at the top level of
	// a delegated turn's own events, and set to the spawning tool_use id
	// inside a subagent's own turn — see the package doc's event-mapping
	// section. Carried verbatim onto the appended message.Message
	// (Message.ParentToolUseID) by claudeCodeAssistantMessage/
	// claudeCodeToolResultMessage.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	// TTFTMillis and DurationMillis are a "result" event's own timing
	// fields (time to first token, and this turn's total wall time),
	// forwarded into emitTurnMetrics's TTFTMillis/StreamMillis — see the
	// package doc's "result" bullet. Zero, never an error, if a particular
	// `claude` build does not send them (this file's usual permissive-
	// decoding philosophy).
	TTFTMillis     int64 `json:"ttft_ms,omitempty"`
	DurationMillis int64 `json:"duration_ms,omitempty"`
	// RateLimitInfo is a "rate_limit_event" envelope's own payload — see
	// mapClaudeCodeRateLimit. nil for every other event type.
	RateLimitInfo *claudeCodeRateLimitInfo `json:"rate_limit_info,omitempty"`
}

// claudeCodeRateLimitInfo is a "rate_limit_event" envelope's own
// rate_limit_info object — the `claude` CLI's subscription rate-limit/quota
// signal, typically the SECOND stream-json message of every turn. See
// mapClaudeCodeRateLimit for how this becomes message.SubscriptionUsage.
type claudeCodeRateLimitInfo struct {
	Status          string                               `json:"status,omitempty"`
	ResetsAt        int64                                `json:"resetsAt,omitempty"`
	RateLimitType   string                               `json:"rateLimitType,omitempty"`
	OverageStatus   string                               `json:"overageStatus,omitempty"`
	OverageResetsAt int64                                `json:"overageResetsAt,omitempty"`
	IsUsingOverage  bool                                 `json:"isUsingOverage,omitempty"`
	UnifiedWindows  map[string]claudeCodeRateLimitWindow `json:"unifiedWindows,omitempty"`
}

// claudeCodeRateLimitWindow is one entry of a claudeCodeRateLimitInfo's own
// unifiedWindows map — one rate-limit window (e.g. "five_hour",
// "seven_day"), keyed by the CLI's own window name.
type claudeCodeRateLimitWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
}

// claudeCodeRateLimitWindowLabel maps a unifiedWindows key to the human
// label message.SubscriptionUsageWindow.Label reports — the two keys a real
// `claude` binary sends today. An unrecognized key (a future CLI addition
// this file has not seen) falls back to the key itself: an honest label
// beats a hardcoded guess for a window this file cannot yet name.
func claudeCodeRateLimitWindowLabel(key string) string {
	switch key {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "Weekly"
	default:
		return key
	}
}

// mapClaudeCodeRateLimit converts a "rate_limit_event" envelope's own
// rate_limit_info object into message.SubscriptionUsage: provider "claude";
// Plan left "" (the CLI's event carries no plan field, and this file does
// not shell out to `claude auth status` just to learn one — see this
// package's own CONSTRAINTS); one window per unifiedWindows entry, sorted
// by key for byte-stable output across turns (map iteration order is not);
// Overage is set only when the event actually carries one — IsUsingOverage
// true, or a non-empty OverageStatus (a status can legitimately describe an
// overage state, e.g. "approaching", even while IsUsingOverage is still
// false) — so a turn with no overage in play leaves it nil, matching
// message.SubscriptionUsage.Overage's own doc comment (omitted on the
// wire, never a hollow zero-value object). CapturedAt is left zero —
// applySubscriptionUsage stamps it from s.cfg.Now(), the single clock
// every consumer of Session.SubscriptionUsage sees. ok is false for a nil
// info (a rate_limit_event with no rate_limit_info at all — not expected
// from a real `claude` binary, but this file decodes permissively
// throughout, per its own package doc).
func mapClaudeCodeRateLimit(info *claudeCodeRateLimitInfo) (message.SubscriptionUsage, bool) {
	if info == nil {
		return message.SubscriptionUsage{}, false
	}
	keys := make([]string, 0, len(info.UnifiedWindows))
	for k := range info.UnifiedWindows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	windows := make([]message.SubscriptionUsageWindow, 0, len(keys))
	for _, k := range keys {
		w := info.UnifiedWindows[k]
		windows = append(windows, message.SubscriptionUsageWindow{
			Key:         k,
			Label:       claudeCodeRateLimitWindowLabel(k),
			UsedPercent: w.Utilization * 100,
			ResetsAt:    w.ResetsAt,
		})
	}
	out := message.SubscriptionUsage{
		Provider: "claude",
		Windows:  windows,
	}
	if info.IsUsingOverage || info.OverageStatus != "" {
		out.Overage = &message.SubscriptionOverage{
			InUse:    info.IsUsingOverage,
			Status:   info.OverageStatus,
			ResetsAt: info.OverageResetsAt,
		}
	}
	return out, true
}

// claudeCodeUsage is a "result" event's usage object.
//
// # Usage mapping
//
// InputTokens and OutputTokens map onto provider.Usage's identically-named
// fields directly. CacheReadInputTokens/CacheCreationInputTokens map onto
// provider.Usage.CacheReadTokens/CacheWriteTokens — the naming differs
// (Claude Code mirrors the raw Anthropic Messages API's own
// cache_read_input_tokens/cache_creation_input_tokens field names; harness's
// provider.Usage instead follows provider/anthropic's own CacheRead/
// CacheWrite convention) but the ACCOUNTING is identical: both are token
// counts for a cache read and a cache write, respectively, over the same
// underlying Anthropic billing model. See mapClaudeCodeUsage.
type claudeCodeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// mapClaudeCodeUsage converts a "result" event's usage object to
// provider.Usage — see claudeCodeUsage's own doc comment for the field-by-
// field mapping and cache-naming reconciliation. A nil u (a result event
// with no usage object at all — not expected from a real `claude` binary,
// but this file decodes permissively throughout) yields the zero Usage
// rather than a nil-dereference panic.
func mapClaudeCodeUsage(u *claudeCodeUsage) provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	return provider.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// claudeCodeMessage is the "message" field an "assistant" or "user"
// stream-json event carries — the same shape the raw Anthropic Messages
// API uses for either role. Content may be a bare JSON string (a plain-
// text-only message, which some SDK code paths emit) or an array of
// claudeCodeContentBlock — see decodeClaudeCodeContentBlocks, which
// accepts both.
type claudeCodeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// claudeCodeContentBlock is one content block of a claudeCodeMessage. Only
// the fields relevant to the block's own Type are ever populated; the rest
// zero-value harmlessly.
type claudeCodeContentBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result"
	// Text is set on a "text" block.
	Text string `json:"text,omitempty"`
	// ID and Name are set on a "tool_use" block; Input is that tool
	// call's arguments object.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID, Content, and IsError are set on a "tool_result" block.
	// Content, like claudeCodeMessage's own field, may be a bare string or
	// an array of blocks — see claudeCodeContentText.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// Thinking and Signature are set on a "thinking" block — the raw
	// Anthropic Messages API shape Claude Code's own "assistant" events
	// reuse verbatim (see provider/anthropic/anthropic.go's identical
	// content_block_start/content_block_delta fields for the API this
	// mirrors). Signature is opaque, provider-native reasoning state,
	// carried into message.Reasoning.ProviderData rather than dropped —
	// see claudeCodeAssistantMessage's "thinking" case and
	// claudeCodeReasoningProviderData.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// decodeClaudeCodeContentBlocks decodes raw as either a JSON array of
// claudeCodeContentBlock or, if that fails, a bare JSON string (treated as
// a single text block) — see claudeCodeMessage.Content's own doc comment.
// Any other or empty shape yields nil, never an error: this file never
// fails a turn over one unrecognized content shape (see the package doc's
// permissive-decoding philosophy).
func decodeClaudeCodeContentBlocks(raw json.RawMessage) []claudeCodeContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []claudeCodeContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return []claudeCodeContentBlock{{Type: "text", Text: text}}
	}
	return nil
}

// claudeCodeContentText flattens a tool_result block's own Content field
// (bare string, or an array of blocks each contributing its own Text) into
// one string, newline-joining multiple blocks. An unrecognized shape falls
// back to the raw JSON bytes verbatim, rather than silently discarding
// content the CLI genuinely sent.
func claudeCodeContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if blocks := decodeClaudeCodeContentBlocks(raw); blocks != nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return string(raw)
}

// claudeCodeReasoningFamily tags a "thinking" block's opaque Signature under
// message.Reasoning.ProviderData, keyed the same way provider/anthropic's
// own Family constant names it ("anthropic") — a delegated turn's thinking
// blocks are Anthropic's own wire shape verbatim (see
// claudeCodeContentBlock's "thinking"/"signature" doc comment), so tagging
// them under the same family a future transcode of this history would
// expect is the honest key, even though this file cannot import
// provider/anthropic to reference its constant directly (package engine
// does not import a specific provider package — see
// ClaudeCodeProviderFamily's own doc comment for the same duplication-over-
// import precedent with provider/claudecode.Family).
const claudeCodeReasoningFamily = "anthropic"

// claudeCodeReasoningData is the JSON shape stored under
// message.Reasoning.ProviderData[claudeCodeReasoningFamily] — mirrors
// provider/anthropic/transcode.go's anthropicReasoningData one-for-one
// (Signature only; a delegated turn never sees a "redacted_thinking" block
// on its own stream-json output, so there is no Redacted field to carry).
type claudeCodeReasoningData struct {
	Signature string `json:"signature,omitempty"`
}

// claudeCodeAssistantMessage decodes an "assistant" event's message field
// into a canonical message.Message: one Reasoning part per "thinking"
// block, one Text part per non-empty "text" content block, one ToolCall
// part per "tool_use" block, in the CLI's own order. parentToolUseID rides
// straight onto the returned Message (see claudeCodeEnvelope.
// ParentToolUseID's own doc comment) — empty for a top-level delegated
// turn's own messages. A decode failure or a message with no recognized
// blocks yields a Message with a nil Parts, which consumeClaudeCodeStream's
// caller treats as "nothing to append".
func claudeCodeAssistantMessage(raw json.RawMessage, model message.ModelRef, parentToolUseID string) message.Message {
	var cm claudeCodeMessage
	_ = json.Unmarshal(raw, &cm) // best-effort; a failure just yields no blocks below
	var parts message.Parts
	for _, b := range decodeClaudeCodeContentBlocks(cm.Content) {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, &message.Text{Text: b.Text})
			}
		case "thinking":
			data, _ := json.Marshal(claudeCodeReasoningData{Signature: b.Signature})
			parts = append(parts, &message.Reasoning{
				Text:         b.Thinking,
				ProviderData: message.ProviderData{claudeCodeReasoningFamily: data},
			})
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			parts = append(parts, &message.ToolCall{CallID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	return message.Message{
		ID:              newID("msg"),
		Role:            message.RoleAssistant,
		Parts:           parts,
		Model:           model,
		Origin:          message.OriginClaudeCode,
		CreatedAt:       time.Now().UTC(),
		ParentToolUseID: parentToolUseID,
	}
}

// claudeCodeToolResultMessage decodes a "user" event's message field —
// Claude Code's own tool_result delivery, in the raw Anthropic API's
// "user"-role convention — into a canonical RoleTool message.Message: one
// ToolResult part per "tool_result" content block. parentToolUseID rides
// onto the returned Message exactly like claudeCodeAssistantMessage's own
// parameter. Returns nil when the message decodes to no tool_result blocks
// at all (an ordinary human-authored "user" event never reaches this
// driver — Session.History's tail is the only user input a delegated turn
// ever sends, over stdin, not stdout — so an empty result here means an
// unrecognized shape, not a real turn boundary to silently drop).
func claudeCodeToolResultMessage(raw json.RawMessage, parentToolUseID string) *message.Message {
	var cm claudeCodeMessage
	if err := json.Unmarshal(raw, &cm); err != nil {
		return nil
	}
	var parts message.Parts
	for _, b := range decodeClaudeCodeContentBlocks(cm.Content) {
		if b.Type != "tool_result" {
			continue
		}
		parts = append(parts, &message.ToolResult{
			CallID:  b.ToolUseID,
			Content: message.Parts{&message.Text{Text: claudeCodeContentText(b.Content)}},
			IsError: b.IsError,
		})
	}
	if len(parts) == 0 {
		return nil
	}
	return &message.Message{
		ID:              newID("msg"),
		Role:            message.RoleTool,
		Parts:           parts,
		Origin:          message.OriginClaudeCode,
		CreatedAt:       time.Now().UTC(),
		ParentToolUseID: parentToolUseID,
	}
}

// claudeCodeEffortArg maps harness's message.Effort to the `claude` CLI's
// own --effort values (low/medium/high — the CLI has no "off"/"minimal"
// level of its own). EffortOff and EffortMinimal both collapse onto the
// CLI's floor, "low" — the same "cap at the nearest coarser level the
// target enum actually offers" precedent provider/openai/transcode.go's
// reasoningEffort and provider/anthropic/transcode.go's thinkingBudget
// already follow for their own, differently-shaped target enums (xhigh/max
// are similarly unreachable through harness's four-level Effort enum, so
// there is no higher tier to cap at here). ok is false for
// message.EffortUnset — send no --effort flag at all, mirroring how an
// unset provider.Request.Effort sends no reasoning control to a native
// provider — and for any value message.ParseEffort would not recognize.
func claudeCodeEffortArg(e message.Effort) (string, bool) {
	switch e {
	case message.EffortOff, message.EffortMinimal, message.EffortLow:
		return "low", true
	case message.EffortMedium:
		return "medium", true
	case message.EffortHigh:
		return "high", true
	default:
		return "", false
	}
}

// claudeCodeAppendSystemPrompt joins segments for the CLI's single-value
// option. Repeating the option would make Claude Code keep only the last value.
func claudeCodeAppendSystemPrompt(segments []string) (string, bool) {
	if len(segments) == 0 {
		return "", false
	}
	return strings.Join(segments, "\n\n"), true
}

func claudeCodeAppendPromptArg(arg string) bool {
	return arg == "--append-system-prompt" ||
		strings.HasPrefix(arg, "--append-system-prompt=") ||
		arg == "--append-system-prompt-file" ||
		strings.HasPrefix(arg, "--append-system-prompt-file=")
}

// claudeCodeRetryableClass classifies a "result" event's own reported
// failure — subtype plus the human-readable result text — as provider-
// weather retryable, mirroring how a native adapter classifies an HTTP
// status or inline API-error event (see provider.RetryableClass). This is
// deliberately NOT "every is_error result is retryable": a genuine
// deterministic outcome (max turns reached, a refusal) must still fail
// fast so goal.go's promptTurnWithRetry does not burn its retry budget on
// a request that will fail identically every time — only a signal this
// file can actually name as transient provider weather gets wrapped.
func claudeCodeRetryableClass(subtype, result string) (provider.RetryableClass, bool) {
	hay := strings.ToLower(subtype + " " + result)
	switch {
	case strings.Contains(hay, "rate_limit") || strings.Contains(hay, "rate limit"):
		return provider.RetryableRateLimited, true
	case strings.Contains(hay, "overloaded"):
		return provider.RetryableOverloaded, true
	case subtype == "error_during_execution":
		// The CLI's own catch-all subtype for an infrastructure-side
		// hiccup during its turn (e.g. a transient API error surfaced
		// mid-execution, not a deterministic domain failure) — mirrors
		// provider/anthropic's inline "error" SSE event mapping to
		// RetryableServerError.
		return provider.RetryableServerError, true
	default:
		return "", false
	}
}

// claudeCodeMCPServerLister is the seam runClaudeCodeTurn uses to read a
// session's configured MCP server definitions for --mcp-config. Kept
// separate from the MCPRegistry interface itself (Tools/CallTool/
// CallServerTool) deliberately: extending MCPRegistry would force every
// existing fake implementation across this package, cmd/harness, and
// server to grow a new method just to keep compiling, for a capability
// only this one call site needs. Only *MCPManager (the production
// implementation, see its own Servers method in mcp.go) and any test fake
// that chooses to implement it need to; an s.cfg.MCP that does not (nil, or
// a fake with no reason to care) simply contributes no MCP passthrough,
// the same fail-open philosophy as an MCP server that never connects (see
// engine/mcp.go's package doc).
type claudeCodeMCPServerLister interface {
	Servers() map[string]MCPServerConfig
}

// claudeCodeMCPServers returns reg's configured MCP servers, or nil if reg
// is nil or does not implement claudeCodeMCPServerLister — see that
// interface's own doc comment.
func claudeCodeMCPServers(reg MCPRegistry) map[string]MCPServerConfig {
	lister, ok := reg.(claudeCodeMCPServerLister)
	if !ok {
		return nil
	}
	return lister.Servers()
}

// claudeCodeMCPConfig is the `claude` CLI's own --mcp-config JSON shape: a
// top-level "mcpServers" object of server definitions (see
// https://code.claude.com/docs's MCP configuration file contract) — a
// stdio server names a command/args/env, an HTTP server names a type/url
// and optional headers.
type claudeCodeMCPConfig struct {
	MCPServers map[string]claudeCodeMCPServerSpec `json:"mcpServers"`
}

// claudeCodeMCPServerSpec is one server entry of claudeCodeMCPConfig. Type
// is omitted for a stdio server (the CLI's own default) and "http" for a
// Streamable HTTP server, in which case Command/Args/Env are unset and
// URL/Headers carry the server's endpoint instead — see
// claudeCodeMCPServerSpecFor.
type claudeCodeMCPServerSpec struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// claudeCodeMCPServerSpecFor translates one engine MCPServerConfig into its
// --mcp-config wire shape. Exactly one of spec.Command/spec.URL is ever
// set — see MCPServerConfig's own doc comment; validateMCPServers enforces
// this at config-load time, well before a value can ever reach here — so
// checking URL first and falling through to the stdio shape otherwise
// never mismatches a server's actual kind.
func claudeCodeMCPServerSpecFor(spec MCPServerConfig) claudeCodeMCPServerSpec {
	if spec.URL != "" {
		return claudeCodeMCPServerSpec{Type: "http", URL: spec.URL, Headers: spec.Headers}
	}
	out := claudeCodeMCPServerSpec{Env: claudeCodeMCPServerEnv(spec.Env)}
	if len(spec.Command) > 0 {
		out.Command = spec.Command[0]
		if len(spec.Command) > 1 {
			out.Args = append([]string(nil), spec.Command[1:]...)
		}
	}
	return out
}

// claudeCodeMCPServerEnv converts MCPServerConfig.Env's "KEY=VALUE" argv-
// style entries into the map object --mcp-config's JSON shape expects. An
// entry with no "=" is skipped rather than failing the whole turn over one
// malformed entry — this file's usual permissive-decoding philosophy
// applied to config translation instead of CLI output.
func claudeCodeMCPServerEnv(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// claudeCodeMCPConfigFile writes s's configured MCP servers (if any) to a
// fresh temp file in the CLI's own --mcp-config JSON shape, returning its
// path plus a cleanup func that removes it (always non-nil, a no-op when
// path is ""). A temp FILE, not an inline JSON string on the command line,
// deliberately: an MCPServerConfig can carry Headers/Env holding real
// credential material, and argv is visible to any other process on the box
// (via /proc or ps) — writing to a file only this process's own return
// value names, then removing it once this call returns (the child has
// already read it by then; it only needs the file at startup), keeps that
// material out of the process list. len(servers) == 0 AND no synthetic
// history-server entry (MCP unconfigured, or s.cfg.MCP does not implement
// claudeCodeMCPServerLister — see claudeCodeMCPServers — and
// ClaudeCodeConfig.HTTPBaseURL unset, e.g. a one-shot `harness run`)
// returns "", a no-op cleanup, and a nil error: MCP passthrough is
// opt-in, never a hard requirement for a delegated turn to proceed.
func (s *Session) claudeCodeMCPConfigFile() (path string, cleanup func(), err error) {
	noop := func() {}
	servers := claudeCodeMCPServers(s.cfg.MCP)
	historyURL := s.claudeCodeHistoryServerURL()
	if len(servers) == 0 && historyURL == "" {
		return "", noop, nil
	}
	cfg := claudeCodeMCPConfig{MCPServers: make(map[string]claudeCodeMCPServerSpec, len(servers)+1)}
	for name, spec := range servers {
		cfg.MCPServers[name] = claudeCodeMCPServerSpecFor(spec)
	}
	if historyURL != "" {
		// A synthetic entry, not one of Config.MCP's own servers — see
		// ClaudeCodeConfig.HTTPBaseURL's own doc comment. This rides the
		// same temp-file mechanism as every other server here (never an
		// inline argv value), so HTTPAuthToken's bearer value gets the
		// same argv-visibility protection
		// TestClaudeCodeMCPConfigCredentialsNeverInChildArgv already locks
		// in for an operator-configured server's own credentials.
		spec := claudeCodeMCPServerSpec{Type: "http", URL: historyURL}
		if tok := s.cfg.ClaudeCode.HTTPAuthToken; tok != "" {
			spec.Headers = map[string]string{"Authorization": "Bearer " + tok}
		}
		cfg.MCPServers[claudeCodeToolsServerName] = spec
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", noop, fmt.Errorf("engine: claude-code: encoding --mcp-config: %w", err)
	}
	f, err := os.CreateTemp("", "harness-claude-code-mcp-*.json")
	if err != nil {
		return "", noop, fmt.Errorf("engine: claude-code: creating --mcp-config file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", noop, fmt.Errorf("engine: claude-code: writing --mcp-config file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", noop, fmt.Errorf("engine: claude-code: closing --mcp-config file: %w", err)
	}
	name := f.Name()
	return name, func() { _ = os.Remove(name) }, nil
}

// claudeCodeInputMessage is the stdin stream-json shape this driver writes
// — one line, one turn (see runClaudeCodeTurn's own doc comment on why a
// child is spawned fresh per harness turn rather than kept alive across
// several).
type claudeCodeInputMessage struct {
	Type    string                      `json:"type"`
	Message claudeCodeInputInnerMessage `json:"message"`
}

type claudeCodeInputInnerMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// capBuffer is an io.Writer that retains at most cap bytes, silently
// dropping anything beyond that — used to bound how much of the `claude`
// child's stderr this file holds onto for a diagnostic message (see
// claudeCodeStderrCap), without letting a runaway or malicious child
// exhaust memory buffering an unbounded stream nothing ever reads back in
// full.
//
// mu guards buf because runClaudeCodeTurn's stderr drain goroutine (see
// its own comment on why stderr is read via cmd.StderrPipe rather than
// handed to Cmd as a plain io.Writer) writes here concurrently with the
// main goroutine's eventual String() call once the turn ends — that call
// is deliberately NOT sequenced after the drain goroutine's own exit (see
// runClaudeCodeTurn), so both sides need their own synchronization rather
// than relying on happens-before through some other event.
type capBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cap int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.cap - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	// Report the full length written, per io.Writer's contract (a short
	// count would make the child's own stderr write fail) — the cap only
	// bounds what this file RETAINS, never what the child is allowed to
	// emit.
	return len(p), nil
}

func (c *capBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.buf.String())
}
