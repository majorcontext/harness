package e2e

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/protocol"
)

// traceWiring is the tracing that the runtime host takes: an in-memory provider and the log of the runtime.
type traceWiring struct {
	provider *trace.TracerProvider
	recorder *tracetest.SpanRecorder
	content  bool
	logs     *lockedBuffer
}

// tracedRuntimes maps the config path of a runtime host to its tracing.
var tracedRuntimes sync.Map

// startTraced opens h over a scripted model with tracing on. The serve host exports to a fake OTLP receiver; the
// runtime host records in memory. spans returns the spans once n have ended.
func startTraced(t *testing.T, h host, content bool, steps ...harnesstest.Step) (d *runtimeDriver, fake *harnesstest.Server, spans func(n int) []harnesstest.Span) {
	t.Helper()
	fake = harnesstest.NewAnthropic(t, steps...)
	path := writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil))
	if h.runtime {
		w := &traceWiring{recorder: tracetest.NewSpanRecorder(), content: content, logs: &lockedBuffer{}}
		w.provider = trace.NewTracerProvider(trace.WithSpanProcessor(w.recorder))
		tracedRuntimes.Store(path, w)
		d = h.openIn(t, path, runtimeWorkdir(t, nil), nil).(*runtimeDriver)
		return d, fake, func(n int) []harnesstest.Span {
			testpoll.Until(t, waitBound, "the spans did not end", func() bool { return len(w.recorder.Ended()) >= n })
			var out []harnesstest.Span
			for _, s := range w.recorder.Ended() {
				out = append(out, spanOfSDK(s))
			}
			return out
		}
	}
	otlp := harnesstest.NewOTLP(t)
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": otlp.URL(), "OTEL_BSP_SCHEDULE_DELAY": "10"}
	if content {
		env["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] = "true"
	}
	d = h.openIn(t, path, runtimeWorkdir(t, nil), env).(*runtimeDriver)
	return d, fake, func(n int) []harnesstest.Span {
		testpoll.Until(t, waitBound, "the spans did not arrive", func() bool { return len(otlp.Spans()) >= n })
		return otlp.Spans()
	}
}

func spanOfSDK(s trace.ReadOnlySpan) harnesstest.Span {
	out := harnesstest.Span{Name: s.Name(), Kind: "SPAN_KIND_" + strings.ToUpper(s.SpanKind().String()), TraceID: s.SpanContext().TraceID().String(), SpanID: s.SpanContext().SpanID().String(),
		Attrs: map[string]any{}, Failed: s.Status().Code == 1}
	if p := s.Parent(); p.IsValid() {
		out.ParentSpanID = p.SpanID().String()
	}
	for _, kv := range s.Attributes() {
		out.Attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	for _, l := range s.Links() {
		out.Links = append(out.Links, l.SpanContext.TraceID().String()+"-"+l.SpanContext.SpanID().String())
	}
	for _, e := range s.Events() {
		ev := harnesstest.SpanEvent{Name: e.Name, Attrs: map[string]any{}}
		for _, kv := range e.Attributes {
			ev.Attrs[string(kv.Key)] = kv.Value.AsInterface()
		}
		out.Events = append(out.Events, ev)
	}
	return out
}

// spansNamed returns the spans whose name starts with prefix, in call order.
func spansNamed(spans []harnesstest.Span, prefix string) []harnesstest.Span {
	var out []harnesstest.Span
	for _, s := range spans {
		if strings.HasPrefix(s.Name, prefix) {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b harnesstest.Span) int {
		x, _ := a.Attrs["harness.call_seq"].(int64)
		y, _ := b.Attrs["harness.call_seq"].(int64)
		return int(x - y)
	})
	return out
}

func wantAttrs(t *testing.T, what string, s harnesstest.Span, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got := s.Attrs[k]; got != v {
			t.Errorf("%s: %s = %#v, want %#v\n%v", what, k, got, v, s.Attrs)
		}
	}
}

const rowTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func traceInput(id, text string, extra map[string]any) map[string]any {
	in := map[string]any{"id": id, "parts": []protocol.Part{{Type: protocol.PartText, Text: text}}}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

var toolThenAnswer = []harnesstest.Step{
	{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{
		ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo hi"}}}, Usage: harnesstest.Usage{Input: 11, Output: 7}}},
	{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done", Usage: harnesstest.Usage{Input: 13, Output: 5}}},
}

func TestContractTraceRows(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T, h host)
	}{
		{"a_turn_with_a_tool_call_records_agent_model_and_tool_spans_that_the_request_and_the_log_name", traceRowSpans},
		{"an_input_with_a_traceparent_links_its_turn_and_a_malformed_one_is_refused", traceRowLink},
		{"message_content_is_off_by_default_and_recorded_as_the_details_event_when_on", traceRowContent},
	}
	onHosts(t, func(t *testing.T, h host) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				row.run(t, h)
			})
		}
	})
}

func traceRowSpans(t *testing.T, h host) {
	d, fake, spans := startTraced(t, h, false, toolThenAnswer...)
	id := runTurn(t, d, "go")
	all := spans(4)
	turn, chats, tools := spansNamed(all, "invoke_agent"), spansNamed(all, "chat "), spansNamed(all, "execute_tool ")
	if len(turn) != 1 || len(chats) != 2 || len(tools) != 1 {
		t.Fatalf("spans = %d turn, %d chat, %d tool, want 1, 2, 1: %+v", len(turn), len(chats), len(tools), all)
	}
	if turn[0].Name != "invoke_agent harness" || turn[0].Kind != "SPAN_KIND_INTERNAL" {
		t.Errorf("turn span = %q %q", turn[0].Name, turn[0].Kind)
	}
	wantAttrs(t, "turn", turn[0], map[string]any{"gen_ai.operation.name": "invoke_agent", "gen_ai.agent.name": "harness", "gen_ai.conversation.id": id})
	turnID, _ := turn[0].Attrs["harness.turn_id"].(string)
	if turnID == "" || turn[0].ParentSpanID != "" {
		t.Errorf("turn span: harness.turn_id %q, parent %q, want an id and a root", turnID, turn[0].ParentSpanID)
	}
	lines := awaitLogLines(t, d, "turn_metrics", 2)
	reqs := fake.Requests()
	for i, c := range chats {
		if !strings.HasPrefix(c.Name, "chat ") || c.ParentSpanID != turn[0].SpanID || c.TraceID != turn[0].TraceID {
			t.Errorf("chat %d = %q, parent %q, want a child of the turn", i, c.Name, c.ParentSpanID)
		}
		wantAttrs(t, "chat", c, map[string]any{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic", "gen_ai.conversation.id": id,
			"harness.purpose": "turn", "harness.turn_id": turnID, "harness.call_seq": int64(i + 1)})
		if got, want := reqs[i].Header.Get("traceparent"), "00-"+c.TraceID+"-"+c.SpanID+"-01"; got != want {
			t.Errorf("request %d traceparent = %q, want %q", i, got, want)
		}
		wantFields(t, "turn_metrics", lines[i], map[string]any{"turn_id": turnID, "call_seq": float64(i + 1), "call_id": c.Attrs["harness.call_id"]})
	}
	wantAttrs(t, "first chat", chats[0], map[string]any{"gen_ai.usage.input_tokens": int64(11), "gen_ai.usage.output_tokens": int64(7)})
	if tools[0].Name != "execute_tool bash" || tools[0].ParentSpanID != turn[0].SpanID {
		t.Errorf("tool span = %q, parent %q, want execute_tool bash under the turn", tools[0].Name, tools[0].ParentSpanID)
	}
	wantAttrs(t, "tool", tools[0], map[string]any{"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "bash", "gen_ai.tool.call.id": "toolu_1", "gen_ai.tool.type": "function"})
}

func traceRowLink(t *testing.T, h host) {
	d, _, spans := startTraced(t, h, false, replyText("ok"))
	id := d.Create(t)
	path := "/sessions/" + id + "/inputs"
	for name, in := range map[string]map[string]any{
		"malformed":          {"traceparent": "00-nothex-00f067aa0ba902b7-01"},
		"all_zero_trace":     {"traceparent": "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		"state_without_head": {"tracestate": "vendor=1"},
	} {
		res := d.call(t, http.MethodPost, path, traceInput("bad_"+name, "hi", in))
		if res.Status != http.StatusBadRequest || errorCode(t, res) != "invalid_request" {
			t.Errorf("%s: response = %d %v, want 400 invalid_request", name, res.Status, res.Body)
		}
	}
	if msgs := provPage(t, d, id); len(msgs) != 0 {
		t.Errorf("a refused input left messages: %+v", msgs)
	}
	d.expect(t, http.StatusCreated, http.MethodPost, path, traceInput("in1", "hi", map[string]any{"traceparent": rowTraceparent, "tracestate": "vendor=1"}), nil)
	d.expect(t, http.StatusOK, http.MethodPost, path, traceInput("in1", "hi", map[string]any{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b8-01", "tracestate": "vendor=1"}), nil)
	d.WaitIdle(t, id)
	turn := spansNamed(spans(2), "invoke_agent")
	if len(turn) != 1 || !slices.Equal(turn[0].Links, []string{"4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7"}) || turn[0].TraceID == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("turn spans = %+v, want one new trace linked to the traceparent of the input", turn)
	}
}

func traceRowContent(t *testing.T, h host) {
	for _, content := range []bool{false, true} {
		d, _, spans := startTraced(t, h, content, toolThenAnswer...)
		runTurn(t, d, "go")
		all := spans(4)
		chat, tool := spansNamed(all, "chat ")[0], spansNamed(all, "execute_tool ")[0]
		i := slices.IndexFunc(chat.Events, func(e harnesstest.SpanEvent) bool { return e.Name == tracing.DetailsEvent })
		if !content {
			for _, s := range all {
				for k := range s.Attrs {
					if strings.HasPrefix(k, "gen_ai.input") || strings.HasPrefix(k, "gen_ai.output") || strings.HasPrefix(k, "gen_ai.tool.call.") && k != "gen_ai.tool.call.id" {
						t.Errorf("content off: span %q has %s", s.Name, k)
					}
				}
			}
			if i >= 0 {
				t.Errorf("content off: the chat span has the details event")
			}
			continue
		}
		if i < 0 {
			t.Fatalf("chat span events = %+v, want the details event", chat.Events)
		}
		in, _ := chat.Events[i].Attrs["gen_ai.input.messages"].(string)
		if !strings.Contains(in, `"role":"user"`) || !strings.Contains(in, `"content":"go"`) {
			t.Errorf("gen_ai.input.messages = %s, want the user message", in)
		}
		if _, ok := chat.Events[i].Attrs["gen_ai.tool.definitions"].(string); !ok {
			t.Errorf("details event attrs = %v, want the tool definitions", chat.Events[i].Attrs)
		}
		if args, _ := tool.Attrs["gen_ai.tool.call.arguments"].(string); !strings.Contains(args, "echo hi") {
			t.Errorf("gen_ai.tool.call.arguments = %q, want the command", args)
		}
	}
}
