// This file implements harness's own hosted MCP tools: get_conversation_history
// and, when configured, the native `process`, `task`, and `model` tools.
//
// # Why this exists
//
// A session delegated to the Claude Code CLI (engine.ClaudeCodeProviderFamily
// — see engine/claude_code_backend.go) drives its turn entirely inside the
// `claude` binary's own stream-json protocol, which has no way to SEED prior
// conversation history: a stream-json "user" input line gets live
// re-executed by the CLI (expensive and nondeterministic, not a history
// replay), and an "assistant" input line is either silently dropped or
// crashes the CLI outright. So a session that switches to claude-code
// mid-conversation — or reaches its first-ever claude-code turn with
// native-loop history already behind it — has no way to hand that history
// to the CLI on stdin.
//
// The fix is pull, not push: this file registers get_conversation_history
// on the per-session Streamable HTTP endpoint POST /session/{id}/mcp (see
// handleSessionMCP and its routes() entry), backed by package mcpserver's
// generic JSON-RPC dispatch. The delegated `claude` process is handed this
// endpoint in its own --mcp-config (see engine.ClaudeCodeConfig.HTTPBaseURL
// and claudeCodeMCPConfigFile) and, on its first delegated turn with prior
// history to catch up on, a short --append-system-prompt directive
// (engine's claudeCodeHistoryDirective) tells it to call the tool before
// answering. Once it does, the tool_result lands in the CLI's OWN session
// — --resume then carries it forward on every later turn for free, so this
// tool needs to be called at most once per delegated session, not once per
// turn.
//
// # Beyond history: a generalized harness-tools server
//
// The same endpoint also advertises three more of harness's native session
// tools when the session has each configured: `process` (engine/process.go,
// gated on Config.Processes non-nil) — the tool a box's `pnpm dev`-style
// long-lived processes are started, stopped, and inspected through; `task`
// (engine/task_tool.go, gated on Config.SessionManager) — spawn/cancel/
// send/status/log against a child session, INCLUDING a model override
// naming a different provider family than the one driving this delegated
// turn (a claude-code-lane agent can spawn a `sol`/`codex`/any-configured
// child this way); and `model` (engine/model_tool.go, gated on
// Config.ModelTool) — but ONLY its list action (the configured provider
// families and aliases to pick a target from), never the engine's own
// status/set: this surface deliberately narrows `model` down from its full
// ToolDef (see modelToolShimInputSchema/modelListOnlyMCPHandler) because
// set re-points THIS session's own live model — a real hijack of whichever
// lane is driving this very delegated turn, not merely an unwanted read —
// and status leaks current-session state a delegated caller has no
// legitimate need for; list is the one action such a caller actually
// needs, to pick a family for task's own spawn(model:...) override
// instead. A delegated claude-code turn otherwise has NO way to reach any
// of the three: it drives its own tool loop entirely inside the `claude`
// binary, never through this package's native runToolCall path, so
// without an MCP entry a delegated turn simply could not manage a
// process, delegate to a subagent, or discover a model family to delegate
// to at all.
// tools/call routes through engine.Session.RunTool (see its own doc
// comment), the SAME generic external-dispatch seam a future harness-hosted
// tool would use — this file deliberately exposes ONLY these four tools,
// not the redundant file tools (read/write/edit/glob/grep/ls — a delegated
// `claude` process already has its own, native equivalents) or the
// remaining loop-internal ones (session_info, goal, mcp, read_tool_result —
// still meaningless outside the native agentic loop this MCP surface exists
// to route AROUND; task and model, by contrast, are genuinely useful to a
// delegated caller and are exposed above).
//
// task's spawn action is already non-blocking (SessionManager.Spawn returns
// the child's session id at once and runs its turn in a goroutine) and its
// status/log actions already pull the child's live status/result directly
// from its own settled node state (SessionManager.DescendantInfo/
// DescendantTranscript) rather than from the native push-delivery path (the
// EngineContext notification a child's completion normally delivers to its
// parent's NEXT native-loop turn) — a delegated turn has no such next turn
// this MCP surface would ever see, so status/log's pull semantics are what
// make collecting a spawned child's result possible here at all. Neither
// property required any change to engine/task_tool.go itself; see
// engine.TaskToolName's own doc comment for the one export this file
// needed.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/mcp"
	"github.com/majorcontext/harness/mcpserver"
	"github.com/majorcontext/harness/message"
)

const (
	// historyToolName is the MCP tool name advertised by tools/list and
	// matched by tools/call — also the exact string engine's
	// claudeCodeHistoryDirective tells the CLI to call.
	historyToolName = "get_conversation_history"

	// historyDefaultLimit and historyMaxLimit bound how many messages one
	// tools/call answers with — see flattenHistory. Default is generous
	// enough that an ordinary session's whole history fits in one call;
	// Max prevents a single call from building an unbounded response for
	// a pathologically long session (the caller pages with offset
	// instead — see historyResultText's "more history available" hint).
	historyDefaultLimit = 500
	historyMaxLimit     = 2000

	// historyContentTruncateBytes bounds how much of any single tool
	// call's arguments or a single tool result's content this file
	// inlines per line — mirrors claude_code_backend.go's
	// claudeCodeStderrCap precedent: a useful summary without letting one
	// oversized tool call/result dominate (or blow up) the whole
	// response.
	historyContentTruncateBytes = 2000
)

// historyToolInputSchema is get_conversation_history's tools/list
// InputSchema: two optional integers, offset/limit, paginating over the
// session's message history oldest-first — see flattenHistory's own doc
// comment for the exact semantics.
var historyToolInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"offset": {
			"type": "integer",
			"minimum": 0,
			"description": "Number of messages (oldest first) to skip before the returned page. Defaults to 0. Use the next_offset value from a prior call's response to continue reading a long history."
		},
		"limit": {
			"type": "integer",
			"minimum": 1,
			"description": "Maximum number of messages to return in this call. Defaults to 500."
		}
	}
}`)

// historyToolDescription is shown to the model in tools/list — it must
// stand on its own even without the --append-system-prompt directive
// (engine's claudeCodeHistoryDirective), since a later turn on a resumed
// CLI session never gets that directive again but can still see this tool
// in its own tools/list cache.
const historyToolDescription = "Read the PRIOR conversation history for this session: messages that already happened before this turn, either on a different model or in a part of this conversation you have not seen. Call this once, before responding, whenever you are continuing a conversation you have not already read. Supports offset/limit pagination for long histories."

// historyToolAnnotations, processToolAnnotations, taskToolAnnotations, and
// modelToolAnnotations are each tool's mcp.Tool.Annotations object (the
// spec's ToolAnnotations hints,
// https://modelcontextprotocol.io/specification/2025-11-25/server/tools#annotations)
// — 2026-era MCP client UIs surface these to a human (or gate a
// destructive call behind confirmation), so an honest hint here is worth
// setting even though this server does not enforce any of them itself.
//
// get_conversation_history is read-only by construction (flattenHistory
// never mutates sess). `process`'s start/stop/restart actions can kill a
// running process, so it gets destructiveHint instead of readOnlyHint.
// `task` bundles spawn/cancel/send (each mutates a session — but only ones
// the CALLER itself spawned, directly or transitively; see task_tool.go's
// own cancel/send doc comments) alongside read-only status/log, so it gets
// readOnlyHint false rather than destructiveHint: a task call can create or
// stop the caller's OWN subagents, never touch anything outside that
// caller-owned subtree, which is a materially smaller blast radius than
// `process`'s ability to kill a shared, box-wide long-lived process.
// `model` is readOnlyHint true — and that is honest ONLY because this
// file's shim restricts the exposed `model` surface to the list action
// (see modelToolShimInputSchema/modelListOnlyMCPHandler below); the
// engine's own `model` tool also has a mutating set action (SetModel:
// persistModel + EventModelChanged), which would make readOnlyHint a lie
// if this shim ever passed it through.
var (
	historyToolAnnotations = json.RawMessage(`{"readOnlyHint": true}`)
	processToolAnnotations = json.RawMessage(`{"destructiveHint": true}`)
	taskToolAnnotations    = json.RawMessage(`{"readOnlyHint": false}`)
	modelToolAnnotations   = json.RawMessage(`{"readOnlyHint": true}`)
)

// modelToolShimInputSchema and modelToolShimDescription are the `model`
// tool's SHIM-published tools/list surface — DELIBERATELY NOT the engine's
// own full ToolDef (status/set/list), unlike process/task just below. A
// delegated caller only ever needs to enumerate provider families/aliases
// to pick one for task's own spawn(model:...) override; it must never
// reach set (re-points THIS session's own main model — the parent's live
// delegation to whatever lane is driving this very turn, a real behavioral
// hijack, not merely an unwanted read) or status (leaks this session's own
// current-model state, which list's own result deliberately omits — see
// modelListResult, engine/model_tool.go). Restricting the PUBLISHED schema
// alone is not enough — a caller can send whatever action it wants
// regardless of what tools/list advertised — so modelListOnlyMCPHandler
// enforces the same restriction on every actual tools/call, before
// dispatch.
var modelToolShimInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["list"], "description": "The operation to perform; only \"list\" is exposed on this surface"}
	},
	"required": ["action"]
}`)

const modelToolShimDescription = "List the provider families and aliases configured on this box. Use this to pick a family for task's own spawn(model:...) override when delegating to a child session. This surface exposes ONLY the list action — inspecting or changing THIS session's own current model is not available here; task's model override is the way to select a model, for a CHILD session, not this one."

// newSessionMCPRegistry builds the per-request mcpserver.Registry for
// sess's own /session/{id}/mcp endpoint (handleSessionMCP): always
// get_conversation_history, plus the native `process`, `task`, and `model`
// tools whenever sess has each one configured (Config.Processes non-nil,
// Config.SessionManager set, Config.ModelTool true, respectively — see
// engine.Session.ToolDef) — see this file's package doc for why exactly
// these four. version is reported as the MCP server's own implementation
// version (Options.Version — the same harness build version every other
// endpoint already reports, not a version of the tool's own wire shape).
func newSessionMCPRegistry(sess *engine.Session, version string) *mcpserver.Registry {
	// "harness-tools" is this server's own self-reported Implementation.Name
	// (initialize's ServerInfo) — cosmetic identification only, unrelated
	// to (if conveniently matching) engine's claudeCodeToolsServerName,
	// the --mcp-config MAP KEY the delegated `claude` process's own client
	// uses to reach this endpoint at all.
	reg := mcpserver.NewRegistry("harness-tools", version)
	reg.SetInstructions("Call get_conversation_history before responding if you have not already read this session's prior conversation history.")
	reg.RegisterTool(mcp.Tool{
		Name:        historyToolName,
		Description: historyToolDescription,
		InputSchema: historyToolInputSchema,
		Annotations: historyToolAnnotations,
	}, historyToolHandler(sess))

	// def for process/task below comes from the engine's OWN tool
	// registration via ToolDef, never a second, hand-duplicated copy of
	// its Description/InputSchema — the two would otherwise be free to
	// silently drift apart. ok is false exactly when the tool's owning
	// Config field is unset (see each ToolDef's own doc comment), the same
	// condition that hides the native tool from the model entirely — this
	// MCP surface must not advertise a tool a delegated turn could call
	// and get "unknown tool" back from RunTool.
	if def, ok := sess.ToolDef(engine.ProcessToolName); ok {
		reg.RegisterTool(mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
			Annotations: processToolAnnotations,
		}, runToolMCPHandler(sess, engine.ProcessToolName))
	}
	if def, ok := sess.ToolDef(engine.TaskToolName); ok {
		reg.RegisterTool(mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
			Annotations: taskToolAnnotations,
		}, runToolMCPHandler(sess, engine.TaskToolName))
	}
	// model is the ONE exception to the "pass the engine's own ToolDef
	// through verbatim" rule above: ok still gates on the real ToolDef (so
	// this surface disappears exactly when Config.ModelTool is off,
	// matching the native tool's own availability), but the Description/
	// InputSchema this file PUBLISHES are the hand-written, list-only
	// modelToolShimDescription/modelToolShimInputSchema, never the
	// engine's full status/set/list schema — see their own doc comment for
	// why passing that through would misrepresent (and worse, invite) a
	// mutating call this shim must never allow.
	if _, ok := sess.ToolDef(engine.ModelToolName); ok {
		reg.RegisterTool(mcp.Tool{
			Name:        engine.ModelToolName,
			Description: modelToolShimDescription,
			InputSchema: modelToolShimInputSchema,
			Annotations: modelToolAnnotations,
		}, modelListOnlyMCPHandler(sess))
	}
	return reg
}

// historyToolCallArgs is get_conversation_history's tools/call arguments
// shape — see historyToolInputSchema.
type historyToolCallArgs struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// historyToolHandler returns the mcpserver.ToolHandler for
// get_conversation_history, closing over sess. It re-reads sess.History()
// on every call (never cached): the session may keep progressing between
// a tools/call and the process's own lifetime, and this handler has no
// reason to serve a stale snapshot.
func historyToolHandler(sess *engine.Session) mcpserver.ToolHandler {
	return func(_ context.Context, raw json.RawMessage) (mcp.CallToolResult, error) {
		var args historyToolCallArgs
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return mcp.CallToolResult{}, fmt.Errorf("%s: invalid arguments: %w", historyToolName, err)
			}
		}
		page := flattenHistory(sess.History(), args.Offset, args.Limit)
		return mcp.CallToolResult{
			Content: []mcp.Content{{Type: mcp.ContentTypeText, Text: historyResultText(page)}},
		}, nil
	}
}

// runToolMCPHandler returns the mcpserver.ToolHandler for the native
// session tool registered under name (process, task, or model — see
// newSessionMCPRegistry), closing over sess. It routes every call through
// sess.RunTool — the SAME generic dispatch path a native-loop call to that
// tool goes through (hooks, events, panic recovery included) — never a
// hand-rolled second implementation of the tool's own action switch.
// RunTool's error already carries the tool-level failure text (e.g. "no
// such process", "unknown action"), and returning it directly from this
// handler makes mcpserver.Registry's own dispatch turn it into
// CallToolResult.IsError automatically — see ToolHandler's own doc comment
// for that contract.
func runToolMCPHandler(sess *engine.Session, name string) mcpserver.ToolHandler {
	return func(ctx context.Context, raw json.RawMessage) (mcp.CallToolResult, error) {
		parts, err := sess.RunTool(ctx, name, raw)
		if err != nil {
			return mcp.CallToolResult{}, err
		}
		return mcp.CallToolResult{Content: partsToMCPContent(parts)}, nil
	}
}

// modelListOnlyMCPHandler returns the mcpserver.ToolHandler for the
// SHIM-restricted `model` tool, closing over sess: it enforces action ==
// "list" itself, BEFORE ever dispatching through RunTool, rather than
// trusting modelToolShimInputSchema's published enum alone — a caller can
// send any action it likes over the wire regardless of what tools/list
// advertised, so the schema is documentation, not enforcement. Rejecting
// here, as an ordinary tool-level error (IsError, not a protocol failure —
// same ToolHandler contract every other handler in this file follows),
// is what actually closes off `set` (re-points THIS session's own live
// model — a real hijack of whichever lane is driving this very delegated
// turn) and `status` (leaks this session's own current-model state) — see
// modelToolShimInputSchema's own doc comment for the full reasoning.
// action == "list" still routes through sess.RunTool(engine.ModelToolName,
// ...) — the SAME generic dispatch path (hooks, events, panic recovery
// included) runToolMCPHandler's other callers use — so nothing about the
// engine's own `model` tool changes; only what THIS shim will forward to
// it is narrowed.
func modelListOnlyMCPHandler(sess *engine.Session) mcpserver.ToolHandler {
	return func(ctx context.Context, raw json.RawMessage) (mcp.CallToolResult, error) {
		var in struct {
			Action string `json:"action"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return mcp.CallToolResult{}, fmt.Errorf("model: invalid arguments: %w", err)
			}
		}
		if in.Action != "list" {
			return mcp.CallToolResult{}, fmt.Errorf("model: action %q is not available on this surface (only \"list\" is exposed here — use task's own model override to select a model for a child session)", in.Action)
		}
		parts, err := sess.RunTool(ctx, engine.ModelToolName, raw)
		if err != nil {
			return mcp.CallToolResult{}, err
		}
		return mcp.CallToolResult{Content: partsToMCPContent(parts)}, nil
	}
}

// partsToMCPContent flattens a native tool's message.Parts result into MCP
// Content items. Every native session tool (process included) answers
// with a single *message.Text part carrying a JSON- or plain-text result
// (see engine/process.go's jsonResult), so Parts.Text()'s own
// newline-joining rule already produces exactly the one string an MCP
// tool_result needs — this wraps it as the transport's required Content
// item rather than reimplementing Parts' own text-extraction logic.
func partsToMCPContent(parts message.Parts) []mcp.Content {
	return []mcp.Content{{Type: mcp.ContentTypeText, Text: parts.Text()}}
}

// historyPage is flattenHistory's result: a rendered page of a session's
// message history plus enough bookkeeping for historyResultText to tell
// the caller where it is and whether to ask for more.
type historyPage struct {
	Text       string
	Total      int
	Offset     int // the actual, clamped starting offset this page begins at
	Returned   int
	NextOffset int
	HasMore    bool
}

// flattenHistory renders msgs[offset:offset+limit] (clamped to msgs'
// bounds) into readable "Role: text" lines — see writeFlattenedMessage for
// the per-role rendering. offset/limit index MESSAGES, oldest first,
// matching Session.History()'s own order; offset < 0 clamps to 0, and
// limit <= 0 or > historyMaxLimit falls back to historyDefaultLimit (an
// absent or malformed pagination arg gets a sane default, never an
// unbounded read or a zero-length response).
//
// A ToolResult's tool NAME (message.ToolResult carries only its CallID) is
// resolved by scanning every ToolCall in msgs BEFORE the page even when
// the matching call itself fell on an earlier page — a caller paging
// through a long history one chunk at a time must still see readable tool
// names on every page, not just the one that happens to include the
// original call.
func flattenHistory(msgs []message.Message, offset, limit int) historyPage {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > historyMaxLimit {
		limit = historyDefaultLimit
	}
	total := len(msgs)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}

	callNames := make(map[string]string)
	for _, m := range msgs[:offset] {
		recordToolCallNames(m, callNames)
	}

	var b strings.Builder
	for _, m := range msgs[offset:end] {
		recordToolCallNames(m, callNames)
		writeFlattenedMessage(&b, m, callNames)
	}

	returned := end - offset
	nextOffset := offset + returned
	return historyPage{
		Text:       b.String(),
		Total:      total,
		Offset:     offset,
		Returned:   returned,
		NextOffset: nextOffset,
		HasMore:    nextOffset < total,
	}
}

func recordToolCallNames(m message.Message, callNames map[string]string) {
	for _, p := range m.Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			callNames[tc.CallID] = tc.Name
		}
	}
}

// writeFlattenedMessage appends one message's own readable rendering to b:
//
//   - RoleUser: "User: <text>" (or "(no text)" for a part-less trigger
//     message — see message.OriginEngine's own doc comment for the one
//     shape that can be this bare).
//   - RoleAssistant: one "Assistant: <text>" line per non-empty Text part,
//     one "Assistant: [thinking]" line per Reasoning part (the reasoning
//     TEXT itself is deliberately omitted — it is verbose, provider-
//     internal narration, not conversational content a catch-up read
//     needs), and one "Assistant called tool NAME(args)" summary per
//     ToolCall part.
//   - RoleTool: one "Tool result (NAME, ok|error): <content>" line per
//     ToolResult part, NAME resolved via callNames (falling back to the
//     bare CallID if no matching ToolCall was ever seen).
//
// Every inlined tool-call-argument or tool-result-content string is
// truncated to historyContentTruncateBytes — see that constant's own doc
// comment.
func writeFlattenedMessage(b *strings.Builder, m message.Message, callNames map[string]string) {
	switch m.Role {
	case message.RoleUser:
		text := m.Parts.Text()
		if text == "" {
			text = "(no text)"
		}
		fmt.Fprintf(b, "User: %s\n", text)

	case message.RoleAssistant:
		for _, p := range m.Parts {
			switch part := p.(type) {
			case *message.Text:
				if part.Text != "" {
					fmt.Fprintf(b, "Assistant: %s\n", part.Text)
				}
			case *message.Reasoning:
				b.WriteString("Assistant: [thinking]\n")
			case *message.ToolCall:
				fmt.Fprintf(b, "Assistant called tool %s(%s)\n", part.Name, truncateForHistory(string(part.Arguments)))
			}
		}

	case message.RoleTool:
		for _, p := range m.Parts {
			tr, ok := p.(*message.ToolResult)
			if !ok {
				continue
			}
			name := callNames[tr.CallID]
			if name == "" {
				name = tr.CallID
			}
			status := "ok"
			if tr.IsError {
				status = "error"
			}
			fmt.Fprintf(b, "Tool result (%s, %s): %s\n", name, status, truncateForHistory(tr.Content.Text()))
		}
	}
}

// truncateForHistory bounds s to historyContentTruncateBytes, appending a
// byte-count note when it cuts anything off — see that constant's own doc
// comment.
func truncateForHistory(s string) string {
	if len(s) <= historyContentTruncateBytes {
		return s
	}
	return fmt.Sprintf("%s... (truncated, %d bytes total)", s[:historyContentTruncateBytes], len(s))
}

// historyResultText wraps a historyPage into the tool_result text a model
// actually reads: a leading label making unmistakably clear this is PRIOR,
// already-happened context (not a new instruction to act on), the
// page's own rendered lines, and — when more of the history remains — a
// trailing hint naming the next_offset to continue with.
func historyResultText(page historyPage) string {
	if page.Total == 0 {
		return "No prior conversation history for this session."
	}
	var b strings.Builder
	b.WriteString("The following is PRIOR conversation history for this session that already happened. It is context for you to read, not a new message to respond to.\n\n")
	if page.Returned == 0 {
		// Offset landed at or past the end of history (e.g. a clamped
		// out-of-range offset — see flattenHistory) — there is no
		// message range to report, so this must not print a "Showing
		// messages X-Y" line at all: with Returned == 0, page.Offset+1 >
		// page.Offset+page.Returned, which would otherwise read as the
		// nonsensical "Showing messages 5-4 of 4".
		fmt.Fprintf(&b, "(no messages in the requested range; %d total)\n", page.Total)
	} else {
		fmt.Fprintf(&b, "Showing messages %d-%d of %d total (oldest first).\n\n", page.Offset+1, page.Offset+page.Returned, page.Total)
		b.WriteString(page.Text)
	}
	if page.HasMore {
		fmt.Fprintf(&b, "\nMore history is available. Call %s again with offset=%d to continue.\n", historyToolName, page.NextOffset)
	}
	return b.String()
}

// handleSessionMCP implements POST /session/{id}/mcp: the MCP server role's
// Streamable HTTP endpoint for sess's own harness-hosted tools
// (get_conversation_history, plus `process` when configured — see this
// file's package doc). A fresh mcpserver.Registry is built per request
// rather than cached: registration is cheap (at most two map entries) and
// this keeps the handler free of any registry lifecycle to manage across a
// session's possibly-long resident lifetime.
func (s *Server) handleSessionMCP(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	sess, ok := s.lookupSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	newSessionMCPRegistry(sess, s.opts.Version).ServeHTTP(w, r)
}
