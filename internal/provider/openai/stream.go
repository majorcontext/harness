package openai

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/provider"
)

// assembledItem accumulates one output item across SSE events, keyed by the
// item's output_index.
type assembledItem struct {
	kind   string // message | function_call | reasoning
	text   bytes.Buffer
	callID string
	name   string
	args   json.RawMessage
	raw    json.RawMessage // reasoning, web_search_call: the entire item JSON, replayed verbatim
	meta   json.RawMessage // message, function_call: the itemMeta that a request sends back
	// query, id and cites are the search of a web_search_call item and the sources a message cites.
	query, id string
	failed    bool
	cites     []message.Citation
}

// stream implements provider.Stream over the Responses API SSE protocol. It
// forwards deltas as they arrive and assembles the canonical assistant
// message, delivered with EventDone on response.completed.
type stream struct {
	body io.Closer
	r    *bufio.Reader
	// wsConn is non-nil for a websocket-delivered response (see wsPool.
	// stream) and nil for the HTTP+SSE path — exactly one of {r, wsConn}
	// is set. Next/Close dispatch on it instead of duplicating the SSE
	// decode loop and event mapping for the ws case: stream.handle below
	// is shared, unmodified, between both transports.
	wsConn *wsFrameSource
	model  modelref.ModelRef
	// family is the client's resolved provider family: the ProviderData tag
	// this stream writes reasoning attachments under. Empty means the
	// package Family constant (see familyOrDefault), which keeps a stream
	// built directly in a test — the fuzz harness does exactly that —
	// behaving as it always has.
	family string

	respID        string
	respCreatedAt time.Time
	items         []*assembledItem
	usage         provider.Usage
	hasToolCall   bool
	// subUsage is this response's captured subscription-usage snapshot —
	// set only for a CodexFamily client, from the HTTP path's response
	// headers (Client.codexSubscriptionUsage) or the ws path's in-band
	// "codex.rate_limits" event (this file's handle), nil otherwise.
	// Carried onto the EventDone event queued in the "response.completed"/
	// "response.incomplete" case below.
	subUsage *message.SubscriptionUsage

	// onComplete publishes transport-local response lineage after stream.handle
	// has assembled a clean response.completed message. It is nil for HTTP.
	onComplete func(responseID string, assistant *message.Message)

	requestMetadata *provider.RequestMetadata
	// recoverChainMiss retries one immediate incremental chain miss as the
	// immutable complete request on the same socket. It is nil for HTTP.
	recoverChainMiss func(first bool, visible bool, chainErr error) (*wsFrameSource, *provider.RequestMetadata, error)
	visibleOutput    bool

	// A consumer joins consecutive reasoning deltas into one block, so
	// reasoning that resumes under a different item or summary part needs a
	// visible break.
	reasoningItem     int
	reasoningSummary  int
	reasoningStreamed bool

	queue []provider.Event
	done  bool
}

// Close releases this stream's transport. For a websocket-delivered
// response, whether the underlying connection is actually torn down or
// kept pooled for the session's next turn was already decided when the
// terminal event was read (see wsFrameSource.close) — Close here just
// tells that decision whether it is being reached cleanly (s.done, the
// same flag Next uses to return io.EOF) or because the caller is
// abandoning the stream early.
func (s *stream) Close() error {
	if s.wsConn != nil {
		return s.wsConn.close(s.done)
	}
	return s.body.Close()
}

// readEvent returns the next (name, data) pair from whichever transport
// this stream is reading — the ws frame source's buffered/live frames, or
// readSSE's HTTP+SSE decode — so handle below never needs to know which.
func (s *stream) readEvent() (string, []byte, error) {
	if s.wsConn != nil {
		return s.wsConn.next()
	}
	return s.readSSE()
}

func (s *stream) Next() (provider.Event, error) {
	for {
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.done {
			return provider.Event{}, io.EOF
		}
		name, data, err := s.readEvent()
		if err != nil {
			// Reaching this read at all means response.completed has not
			// been seen (s.done, checked above, would have returned the
			// normal end-of-iteration io.EOF) — so any read failure here
			// is the stream dying mid-response: transient, classified
			// retryable.
			return provider.Event{}, provider.MarkStreamTruncated(err)
		}
		if err := s.handle(name, data); err != nil {
			var miss *previousResponseNotFoundError
			if errors.As(err, &miss) && s.recoverChainMiss != nil {
				source, metadata, recoverErr := s.recoverChainMiss(s.wsConn.framesRead == 1, s.visibleOutput, err)
				s.recoverChainMiss = nil
				if recoverErr != nil {
					return provider.Event{}, recoverErr
				}
				s.wsConn = source
				s.requestMetadata = metadata
				s.respID = ""
				s.items = nil
				s.usage = provider.Usage{}
				s.hasToolCall = false
				s.subUsage = nil
				s.queue = nil
				continue
			}
			return provider.Event{}, err
		}
		if len(s.queue) == 0 && !s.done {
			// Handled but queued nothing consumer-visible: surface as
			// activity so idle-watchdog consumers see the wire is alive —
			// see provider.EventActivity and internal/provider/anthropic's Next.
			return provider.Event{Type: provider.EventActivity}, nil
		}
	}
}

// readSSE reads one server-sent event: an "event:" line, "data:" lines
// (concatenated), terminated by a blank line.
func (s *stream) readSSE() (name string, data []byte, err error) {
	var buf bytes.Buffer
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			// An event whose blank-line terminator never arrived is
			// DISCARDED, per the SSE spec — see internal/provider/anthropic's
			// readSSE for why handing the fragment up dodged truncation
			// classification.
			return "", nil, err
		}
		line = trimEOL(line)
		switch {
		case line == "":
			if name != "" || buf.Len() > 0 {
				return name, buf.Bytes(), nil
			}
		case line[0] == ':':
			// Keepalive heartbeat comment — see internal/provider/anthropic's
			// readSSE: surface it between events so Next emits
			// EventActivity and idle watchdogs see the wire is alive.
			if name == "" && buf.Len() == 0 {
				return "", nil, nil
			}
		case len(line) > 6 && line[:6] == "event:":
			name = trimSpaceLeft(line[6:])
		case len(line) > 5 && line[:5] == "data:":
			buf.WriteString(trimSpaceLeft(line[5:]))
		}
		// Unknown fields are ignored per the SSE spec.
	}
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func trimSpaceLeft(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return s
}

// maxOutputIndex bounds the output_index values itemAt accepts: the slice
// grows to the index named on the wire, so an unbounded value is an
// attacker/corruption-controlled allocation. FuzzStreamDecode found
// output_index 177777777 forcing a ~1.4GB slice (the nightly fuzz worker
// died of resource exhaustion), and a negative index panicked. Real
// responses carry at most a few dozen output items; 10000 is generous
// beyond any legitimate stream.
const maxOutputIndex = 10000

// itemAt returns the assembled item at output_index idx, growing the slice
// as needed, or an error for an index no legitimate stream produces.
func (s *stream) itemAt(idx int) (*assembledItem, error) {
	if idx < 0 || idx > maxOutputIndex {
		return nil, fmt.Errorf("openai: output_index %d out of range [0, %d]", idx, maxOutputIndex)
	}
	for len(s.items) <= idx {
		s.items = append(s.items, nil)
	}
	if s.items[idx] == nil {
		s.items[idx] = &assembledItem{}
	}
	return s.items[idx], nil
}

type previousResponseNotFoundError struct {
	message string
}

func (e *previousResponseNotFoundError) Error() string {
	return fmt.Sprintf("openai: %s (previous_response_not_found)", e.message)
}

// isNotFoundErrorCode reports whether code identifies a Codex Responses
// rejection meaning "the referenced prior response/conversation no longer
// exists": the API's documented previous_response_not_found code, plus the
// plain HTTP-status vocabulary ("404", "not_found") the same condition has
// also been observed to arrive as. Both streamError and
// isPreviousResponseNotFoundFrame classify on this one predicate, so a chain
// miss recovers the same way regardless of which vocabulary the backend used.
func isNotFoundErrorCode(code string) bool {
	switch code {
	case "previous_response_not_found", "404", "not_found":
		return true
	default:
		return false
	}
}

// isInvalidPreviousResponseMessage reports whether an error message names
// previous_response_id itself. The live ChatGPT Codex backend rejects an
// unusable reference as
//
//	{"type":"error","status":400,"error":{"type":"invalid_request_error",
//	 "message":"Invalid `previous_response_id`."}}
//
// carrying no error code at all, so isNotFoundErrorCode cannot see it and
// the chain miss escapes as a plain, non-retryable turn error. The wire
// field name in the message is the only signal that frame has. Matching it
// stays narrow: "invalid_request_error" alone describes every malformed
// request, and a message naming this one field describes only a request
// whose reference the server would not take. A false positive costs exactly
// one complete re-send on a fresh connection, which is what an unusable
// reference needs anyway.
func isInvalidPreviousResponseMessage(message string) bool {
	return strings.Contains(message, "previous_response_id")
}

func streamError(code, message, typ string) error {
	if isNotFoundErrorCode(code) || isInvalidPreviousResponseMessage(message) {
		if message == "" {
			message = "previous response not found"
		}
		return &previousResponseNotFoundError{message: message}
	}
	msg := fmt.Sprintf("openai: %s", message)
	if code != "" {
		msg = fmt.Sprintf("openai: %s (%s)", message, code)
	}
	if hint, ok := parseUsageExhaustion(0, message, typ, code); ok {
		return provider.MarkPermanent(&provider.Error{
			Kind:        provider.ErrKindProviderExhausted,
			Raw:         msg,
			RecoverHint: hint,
		})
	}
	err := errors.New(msg)
	if code == "" {
		return err
	}
	if class, ok := classifyErrorCode(code); ok {
		return provider.MarkRetryable(err, class)
	}
	return err
}

// withReset gives an exhaustion error the reset time, in Unix seconds, that
// the error body of the Codex backend states in resets_at.
func withReset(err error, resetsAt int64) error {
	if pe, ok := provider.AsProviderExhausted(err); ok && resetsAt > 0 {
		pe.ResetsAt = resetsAt
		pe.RecoverHint = cmp.Or(pe.RecoverHint, provider.ResetHint(resetsAt))
	}
	return err
}

var usageExhaustionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage limit has been reached`),
	regexp.MustCompile(`(?i)usage limit (?:reached|exceeded)`),
}

var usageExhaustionCodes = map[string]bool{
	"insufficient_quota":  true,
	"usage_limit_reached": true,
}

var exhaustionStatuses = map[int]bool{
	http.StatusBadRequest:      true,
	http.StatusPaymentRequired: true,
	http.StatusForbidden:       true,
	http.StatusTooManyRequests: true,
}

// parseUsageExhaustion checks status and code before ever matching the
// message text, mirroring internal/provider/anthropic's parseUsageExhaustion.
func parseUsageExhaustion(status int, message string, codes ...string) (recoverHint string, ok bool) {
	if status > 0 && !exhaustionStatuses[status] {
		return "", false
	}
	matched := false
	for _, c := range codes {
		if usageExhaustionCodes[c] {
			matched = true
			break
		}
	}
	if !matched {
		for _, pat := range usageExhaustionPatterns {
			if pat.MatchString(message) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return "", false
	}
	if m := recoverHintPattern.FindStringSubmatch(message); m != nil {
		return strings.TrimSpace(m[1]), true
	}
	return "", true
}

var recoverHintPattern = regexp.MustCompile(`(?i)regain access on ([^.\n"]+)`)

func isPreviousResponseNotFoundFrame(name string, data []byte) bool {
	if name != "response.failed" && name != "error" {
		return false
	}
	var ev struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Response struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return false
	}
	return isNotFoundErrorCode(ev.Code) ||
		isNotFoundErrorCode(ev.Response.Error.Code) ||
		isNotFoundErrorCode(ev.Error.Code) ||
		isInvalidPreviousResponseMessage(ev.Message) ||
		isInvalidPreviousResponseMessage(ev.Response.Error.Message) ||
		isInvalidPreviousResponseMessage(ev.Error.Message)
}

// reasoningBreak separates two reasoning summaries a consumer joins.
const reasoningBreak = "\n\n"

func (s *stream) handle(name string, data []byte) error {
	switch name {
	case "response.created":
		var ev struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.created: %w", err)
		}
		s.respID = ev.Response.ID
		s.respCreatedAt = time.Now().UTC()

	case "response.output_text.delta":
		var ev struct {
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.output_text.delta: %w", err)
		}
		it, err := s.itemAt(ev.OutputIndex)
		if err != nil {
			return err
		}
		if it.kind == "" {
			it.kind = "message"
		}
		it.text.WriteString(ev.Delta)
		s.visibleOutput = true
		s.queue = append(s.queue, provider.Event{Type: provider.EventTextDelta, Text: ev.Delta, ID: s.respID, CreatedAt: s.respCreatedAt})

	case "response.reasoning_summary_text.delta":
		var ev struct {
			OutputIndex  int    `json:"output_index"`
			SummaryIndex int    `json:"summary_index"`
			Delta        string `json:"delta"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.reasoning_summary_text.delta: %w", err)
		}
		return s.reasoningDelta(ev.OutputIndex, ev.SummaryIndex, ev.Delta)

	case "response.output_item.done":
		var ev struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad response.output_item.done: %w", err)
		}
		var head struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
			Phase     string `json:"phase"`
			Action    struct {
				Query string `json:"query"`
			} `json:"action"`
			Content []struct {
				Annotations []struct {
					Type  string `json:"type"`
					URL   string `json:"url"`
					Title string `json:"title"`
				} `json:"annotations"`
			} `json:"content"`
		}
		if err := json.Unmarshal(ev.Item, &head); err != nil {
			return fmt.Errorf("openai: bad output item: %w", err)
		}
		it, err := s.itemAt(ev.OutputIndex)
		if err != nil {
			return err
		}
		if head.Type == "function_call" || head.Type == "message" {
			it.meta = itemMetaOf(head.ID, head.Status, head.Phase)
		}
		switch head.Type {
		case "function_call":
			it.kind = "function_call"
			it.callID = head.CallID
			it.name = head.Name
			it.args = argsRaw(head.Arguments)
			s.hasToolCall = true
			s.visibleOutput = true
			s.queue = append(s.queue, provider.Event{Type: provider.EventToolCall, ToolCall: it.toolCall()})
		case "reasoning":
			it.kind = "reasoning"
			it.raw = append(json.RawMessage(nil), ev.Item...)
		case "web_search_call":
			it.kind, it.id, it.query, it.failed = "web_search_call", head.ID, head.Action.Query, head.Status == "failed"
			it.raw = append(json.RawMessage(nil), ev.Item...)
			s.visibleOutput = true
		case "message":
			if it.kind == "" {
				it.kind = "message"
			}
			for _, c := range head.Content {
				for _, a := range c.Annotations {
					if a.Type == "url_citation" {
						it.cites = append(it.cites, message.Citation{URL: a.URL, Title: a.Title})
					}
				}
			}
		}

	case codexRateLimitsEventType:
		// An unparseable frame is absent, never a turn failure.
		if familyOrDefault(s.family) == CodexFamily {
			if usage, err := codexSubscriptionUsageFromRateLimitsEvent(data); err == nil && usage != nil {
				s.subUsage = usage
			}
		}

	case "response.completed", "response.incomplete":
		// Both are terminal. incomplete_details.reason of response.incomplete
		// maps to the stop reason.
		var ev struct {
			Response struct {
				ID                string `json:"id"`
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: bad %s: %w", name, err)
		}
		// input_tokens includes the cached portion; Usage wants disjoint parts.
		cached := ev.Response.Usage.InputTokensDetails.CachedTokens
		uncached := ev.Response.Usage.InputTokens - cached
		if uncached < 0 {
			uncached = 0
		}
		s.usage.InputTokens = uncached
		s.usage.OutputTokens = ev.Response.Usage.OutputTokens
		s.usage.CacheReadTokens = cached

		var stop provider.StopReason
		var incomplete string
		switch {
		case name == "response.incomplete":
			stop = mapIncompleteReason(ev.Response.IncompleteDetails.Reason)
			if stop == provider.StopIncomplete {
				incomplete = cmp.Or(ev.Response.IncompleteDetails.Reason, "unknown")
			}
		case s.hasToolCall:
			stop = provider.StopToolUse
		default:
			stop = provider.StopEndTurn
		}
		if ev.Response.ID != "" {
			s.respID = ev.Response.ID
		}
		if s.respCreatedAt.IsZero() {
			s.respCreatedAt = time.Now().UTC()
		}
		assistant := s.assemble()
		if name == "response.completed" && s.onComplete != nil {
			s.onComplete(ev.Response.ID, assistant)
		}
		s.queue = append(s.queue, provider.Event{
			Type:              provider.EventDone,
			Message:           assistant,
			StopReason:        stop,
			IncompleteReason:  incomplete,
			Usage:             s.usage,
			SubscriptionUsage: s.subUsage,
			RequestMetadata:   s.requestMetadata,
		})
		s.done = true

	case "response.failed", "error":
		var ev struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			Response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Error struct {
				Type     string `json:"type"`
				Code     string `json:"code"`
				Message  string `json:"message"`
				ResetsAt int64  `json:"resets_at"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("openai: stream error: %s", data)
		}
		switch {
		case isNotFoundErrorCode(ev.Response.Error.Code):
			return streamError(ev.Response.Error.Code, ev.Response.Error.Message, "")
		case isNotFoundErrorCode(ev.Error.Code):
			return streamError(ev.Error.Code, ev.Error.Message, "")
		case isNotFoundErrorCode(ev.Code):
			return streamError(ev.Code, ev.Message, "")
		case ev.Response.Error.Message != "" || ev.Response.Error.Code != "":
			return streamError(ev.Response.Error.Code, ev.Response.Error.Message, "")
		case ev.Error.Message != "" || ev.Error.Code != "" || ev.Error.Type != "":
			return withReset(streamError(cmp.Or(ev.Error.Code, ev.Code), cmp.Or(ev.Error.Message, ev.Message, ev.Error.Type), ev.Error.Type), ev.Error.ResetsAt)
		case ev.Message != "" || ev.Code != "":
			return streamError(ev.Code, ev.Message, "")
		default:
			return fmt.Errorf("openai: stream error: %s", data)
		}
	}
	return nil
}

// reasoningDelta adds a reasoning summary delta to its item and queues it.
func (s *stream) reasoningDelta(outputIndex, summaryIndex int, delta string) error {
	it, err := s.itemAt(outputIndex)
	if err != nil {
		return err
	}
	if it.kind == "" {
		it.kind = "reasoning"
	}
	newItem := s.reasoningStreamed && outputIndex != s.reasoningItem
	newSummary := s.reasoningStreamed && !newItem && summaryIndex != s.reasoningSummary
	// A new item becomes its own part, so its break belongs in the stream
	// only. A new summary part shares one part and needs the stored break.
	if newSummary {
		it.text.WriteString(reasoningBreak)
	}
	it.text.WriteString(delta)
	if newItem || newSummary {
		delta = reasoningBreak + delta
	}
	s.reasoningItem = outputIndex
	s.reasoningSummary = summaryIndex
	s.reasoningStreamed = true
	s.visibleOutput = true
	s.queue = append(s.queue, provider.Event{Type: provider.EventReasoningDelta, Text: delta, ID: s.respID, CreatedAt: s.respCreatedAt})
	return nil
}

// mapIncompleteReason maps response.incomplete_details.reason to a canonical
// stop reason.
func mapIncompleteReason(reason string) provider.StopReason {
	switch reason {
	case "max_output_tokens":
		return provider.StopMaxTokens
	case "content_filter":
		return provider.StopRefusal
	case "interrupted":
		return provider.StopOther
	default:
		return provider.StopIncomplete
	}
}

func argsRaw(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

func (it *assembledItem) toolCall() *message.ToolCall {
	args := it.args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return &message.ToolCall{
		// The provider's call_id becomes the canonical CallID: it is wire-safe
		// here by construction, so same-provider replay preserves it and keeps
		// the prompt cache warm.
		CallID:    it.callID,
		Name:      it.name,
		Arguments: append(json.RawMessage(nil), args...),
	}
}

// assemble builds the canonical assistant message from accumulated items.
func (s *stream) assemble() *message.Message {
	msg := &message.Message{
		ID:        s.respID,
		Role:      message.RoleAssistant,
		Model:     s.model,
		CreatedAt: s.respCreatedAt,
	}
	for _, it := range s.items {
		if it == nil {
			continue
		}
		switch it.kind {
		case "message":
			if it.text.Len() > 0 {
				msg.Parts = append(msg.Parts, &message.Text{Text: it.text.String(), Citations: it.cites, ProviderData: s.providerData(it.meta)})
			}
		case "web_search_call":
			w := &message.WebSearch{ID: it.id, Query: it.query}
			if it.failed {
				w.Error = "failed"
			} else {
				w.ProviderData.Set(familyOrDefault(s.family), it.raw)
			}
			msg.Parts = append(msg.Parts, w)
		case "reasoning":
			if len(it.raw) == 0 {
				continue
			}
			msg.Parts = append(msg.Parts, &message.Reasoning{
				Text:         it.text.String(),
				ProviderData: message.ProviderData{familyOrDefault(s.family): it.raw},
			})
		case "function_call":
			c := it.toolCall()
			c.ProviderData = s.providerData(it.meta)
			msg.Parts = append(msg.Parts, c)
		}
	}
	return msg
}

// providerData returns meta under the family of the stream, or nil for no meta.
func (s *stream) providerData(meta json.RawMessage) message.ProviderData {
	if len(meta) == 0 {
		return nil
	}
	return message.ProviderData{familyOrDefault(s.family): meta}
}
