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
//   - "assistant": one COMPLETE API-level assistant message (text and/or
//     tool_use content blocks together) — NOT a token-by-token delta; this
//     driver does not pass --include-partial-messages, so there is nothing
//     more granular to forward. Decoded into one message.Message (Text and
//     ToolCall parts, in order), appended via plain Session.append (no
//     usage — see the usage-mapping note below) and emitted as
//     EventMessage, with one EventTextDelta per non-empty text part
//     (folding the whole block's text into the message in a single
//     "delta", the closest honest match to the native EventTextDelta
//     contract given the CLI hands over complete blocks) and one
//     EventToolStart per tool_use part.
//   - "user": Claude Code's own tool execution results, arriving in the
//     Anthropic API's own convention of a "user"-role message carrying
//     tool_result content blocks (Claude Code executes its OWN tools here
//     — this package never calls runToolCalls for a delegated turn).
//     Decoded into one RoleTool message.Message (one ToolResult part per
//     block), appended and emitted as EventMessage, with one EventToolEnd
//     per part.
//   - "result": the turn's terminal event. Never itself appended as a
//     message (the assistant text it summarizes was already appended by
//     the last "assistant" event above) — it instead carries the turn's
//     AGGREGATE usage, applied once via applyClaudeCodeUsage (a durable,
//     message-independent record — see recClaudeCodeUsage in store.go).
//     An IsError result becomes this call's returned error; TotalCostUSD
//     has no home in provider.Usage (no adapter carries a cost field —
//     every consumer derives cost from token counts) and is deliberately
//     dropped, not persisted.
//
// # Deferred for v1 (flagged, not silently skipped)
//
//   - MCP passthrough (`--mcp-config`): a delegated turn does not forward
//     harness's configured MCP servers to the `claude` child. Wiring this
//     correctly means translating engine/mcp.go's server specs into the
//     CLI's own --mcp-config JSON shape and reconciling two independent
//     permission/tool-approval models — real, separable follow-on work.
//   - `--append-system-prompt`: not auto-populated from s.cfg.System.
//     Harness's system-prompt assembly (project instructions, Agent
//     Skills, tool-batching guidance) is deliberately native-loop-only
//     (see PromptWithOrigin's dispatch comment) — Claude Code already
//     discovers its own CLAUDE.md/AGENTS.md in the box workspace, and
//     re-injecting harness's OWN native-tool-shaped instructions into a
//     CLI that has different tools would be actively misleading. An
//     operator who wants extra injected wording can still reach the flag
//     via config.Provider.ExtraArgs.
//   - Full goal-loop support: the directive-reuse retry path (see above)
//     IS dispatched correctly, so a goal loop driving a delegated session
//     does not error out — but goal.go's retryable-error CLASSIFICATION
//     (provider.RetryableError, promptTurnWithRetry's backoff/park
//     decisions) is shaped entirely around native provider.Stream errors.
//     An error this file returns is a plain error, never classified
//     retryable, so a goal loop treats every delegated-turn failure as a
//     deterministic stall rather than transient provider weather. Basic
//     interactive Prompt-driven delegation is the verified, supported
//     shape for v1.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
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
	// ExtraArgs are appended verbatim after every flag this file
	// constructs itself. See config.Provider.ExtraArgs's doc comment for
	// the escape-hatch flags this is for (--append-system-prompt,
	// --mcp-config, --allowedTools, ...).
	ExtraArgs []string
	// PermissionMode, if non-empty, becomes --permission-mode <value>.
	PermissionMode string
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
	text := lastUserMessageText(s.History())
	if text == "" {
		return nil, errors.New("engine: claude-code delegated turn found no pending user message to answer")
	}

	cfg := s.cfg.ClaudeCode
	binary := cfg.BinaryPath
	if binary == "" {
		binary = defaultClaudeCodeBinaryPath
	}
	model := s.Model()

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if model.Model != "" {
		args = append(args, "--model", model.Model)
	}
	if resumeID := s.claudeCodeSessionID(); resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
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
	var stderr capBuffer
	stderr.cap = claudeCodeStderrCap
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("engine: claude-code: starting %q: %w", binary, err)
	}

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
	if _, err := stdin.Write(append(inputLine, '\n')); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("engine: claude-code: writing turn input: %w", err)
	}
	// Close stdin: this driver sends exactly one turn per `claude` child
	// (continuity across harness turns is --resume, not a long-lived
	// child — see this file's package doc), and an unclosed stdin would
	// leave the CLI waiting indefinitely for a second message that is
	// never coming, wedging cmd.Wait() below forever.
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("engine: claude-code: closing stdin: %w", err)
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

	finalMsg, turnErr := s.consumeClaudeCodeStream(stdout, model)

	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		// An abort/shutdown-driven cancellation always wins over whatever
		// the stream decoded — the same precedence the native path's
		// context.Canceled handling gives ctx (engine.go's
		// streamTurnWithRetry/runAgenticLoop treat a canceled context as a
		// deliberate stop, not an ordinary failure).
		return nil, ctx.Err()
	}
	if turnErr != nil {
		return nil, turnErr
	}
	if waitErr != nil {
		msg := fmt.Sprintf("engine: claude-code: %q exited with error: %v", binary, waitErr)
		if tail := stderr.String(); tail != "" {
			msg += fmt.Sprintf(" (stderr: %s)", tail)
		}
		return nil, errors.New(msg)
	}
	if finalMsg == nil {
		return nil, errors.New("engine: claude-code: turn ended with no assistant message")
	}
	return finalMsg, nil
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

// consumeClaudeCodeStream reads newline-delimited stream-json events from
// r (the `claude` child's stdout) until EOF, appending/emitting each
// decoded event per this file's package-doc mapping, and returns the last
// assistant message.Message it appended (nil if none) plus any turn-
// ending error a "result" event's IsError reported.
func (s *Session) consumeClaudeCodeStream(r io.Reader, model message.ModelRef) (*message.Message, error) {
	scanner := bufio.NewScanner(r)
	// A tool call's arguments or a large tool result can exceed
	// bufio.Scanner's 64KiB default token size; 8MiB comfortably covers
	// any realistic single stream-json line without an unbounded read.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var finalMsg *message.Message
	var turnErr error
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
			if env.Subtype == "init" {
				s.recordClaudeCodeSessionID(env.SessionID)
			}
			// Any other subtype (e.g. "api_retry") is observed but
			// requires no action — see the package doc's event-mapping
			// section.
		case "assistant":
			msg := claudeCodeAssistantMessage(env.Message, model)
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
				case *message.ToolCall:
					s.emit(Event{Type: EventToolStart, ToolCall: part})
				}
			}
			finalMsg = &msg
		case "user":
			msg := claudeCodeToolResultMessage(env.Message)
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
			s.applyClaudeCodeUsage(mapClaudeCodeUsage(env.Usage))
			if env.IsError {
				turnErr = fmt.Errorf("engine: claude-code: turn ended in error (subtype %q): %s", env.Subtype, env.Result)
			}
			// TotalCostUSD is intentionally dropped here — see the
			// package doc's "result" bullet: provider.Usage has no cost
			// field for it to occupy.
		}
		// Any other top-level "type" (this driver has none documented
		// beyond the four above) is inert activity, per the package doc.
	}
	return finalMsg, turnErr
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

// claudeCodeAssistantMessage decodes an "assistant" event's message field
// into a canonical message.Message: one Text part per non-empty "text"
// content block, one ToolCall part per "tool_use" block, in the CLI's own
// order. A decode failure or a message with no recognized blocks yields a
// Message with a nil Parts, which consumeClaudeCodeStream's caller treats
// as "nothing to append".
func claudeCodeAssistantMessage(raw json.RawMessage, model message.ModelRef) message.Message {
	var cm claudeCodeMessage
	_ = json.Unmarshal(raw, &cm) // best-effort; a failure just yields no blocks below
	var parts message.Parts
	for _, b := range decodeClaudeCodeContentBlocks(cm.Content) {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, &message.Text{Text: b.Text})
			}
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			parts = append(parts, &message.ToolCall{CallID: b.ID, Name: b.Name, Arguments: args})
		}
	}
	return message.Message{
		ID:        newID("msg"),
		Role:      message.RoleAssistant,
		Parts:     parts,
		Model:     model,
		Origin:    message.OriginClaudeCode,
		CreatedAt: time.Now().UTC(),
	}
}

// claudeCodeToolResultMessage decodes a "user" event's message field —
// Claude Code's own tool_result delivery, in the raw Anthropic API's
// "user"-role convention — into a canonical RoleTool message.Message: one
// ToolResult part per "tool_result" content block. Returns nil when the
// message decodes to no tool_result blocks at all (an ordinary human-
// authored "user" event never reaches this driver — Session.History's
// tail is the only user input a delegated turn ever sends, over stdin, not
// stdout — so an empty result here means an unrecognized shape, not a
// real turn boundary to silently drop).
func claudeCodeToolResultMessage(raw json.RawMessage) *message.Message {
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
		ID:        newID("msg"),
		Role:      message.RoleTool,
		Parts:     parts,
		Origin:    message.OriginClaudeCode,
		CreatedAt: time.Now().UTC(),
	}
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
type capBuffer struct {
	buf bytes.Buffer
	cap int
}

func (c *capBuffer) Write(p []byte) (int, error) {
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

func (c *capBuffer) String() string { return strings.TrimSpace(c.buf.String()) }
