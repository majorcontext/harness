// This file implements the harness-hosted get_conversation_history MCP
// tool.
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
// The fix is pull, not push: this file registers one MCP tool,
// get_conversation_history, on the per-session Streamable HTTP endpoint
// POST /session/{id}/mcp (see handleSessionMCP and its routes() entry),
// backed by package mcpserver's generic JSON-RPC dispatch. The delegated
// `claude` process is handed this endpoint in its own --mcp-config (see
// engine.ClaudeCodeConfig.HTTPBaseURL and claudeCodeMCPConfigFile) and, on
// its first delegated turn with prior history to catch up on, a short
// --append-system-prompt directive (engine's claudeCodeHistoryDirective)
// tells it to call the tool before answering. Once it does, the tool_result
// lands in the CLI's OWN session — --resume then carries it forward on
// every later turn for free, so this tool needs to be called at most once
// per delegated session, not once per turn.

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

// newHistoryRegistry builds the per-request mcpserver.Registry for sess's
// own /session/{id}/mcp endpoint (handleSessionMCP). version is reported
// as the MCP server's own implementation version (Options.Version — the
// same harness build version every other endpoint already reports, not a
// version of the tool's own wire shape).
func newHistoryRegistry(sess *engine.Session, version string) *mcpserver.Registry {
	reg := mcpserver.NewRegistry("harness-history", version)
	reg.SetInstructions("Call get_conversation_history before responding if you have not already read this session's prior conversation history.")
	reg.RegisterTool(mcp.Tool{
		Name:        historyToolName,
		Description: historyToolDescription,
		InputSchema: historyToolInputSchema,
	}, historyToolHandler(sess))
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
// Streamable HTTP endpoint for sess's own harness-hosted tools (currently
// just get_conversation_history — see this file's package doc). A fresh
// mcpserver.Registry is built per request rather than cached: registration
// is cheap (one map entry) and this keeps the handler free of any registry
// lifecycle to manage across a session's possibly-long resident lifetime.
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
	newHistoryRegistry(sess, s.opts.Version).ServeHTTP(w, r)
}
