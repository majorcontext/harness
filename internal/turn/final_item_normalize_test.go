package turn_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type finalItemModel struct{}

func (finalItemModel) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }
func (finalItemModel) Run(_ context.Context, req turn.Request, out turn.Sink) error {
	if len(req.History) > 1 {
		return out.Item(say("done"))
	}
	return out.Item(protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartToolCall, CallID: "1", Name: "probe", Arguments: json.RawMessage(`{"value":"x"}`), ArgumentsText: `{ "value": "x" }`}}})
}

type rawInputProbe struct{ seen chan string }

func (rawInputProbe) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "probe"} }
func (p rawInputProbe) NormalizeInput(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	p.seen <- string(input)
	return input, nil
}
func (rawInputProbe) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: "done"}, nil
}

func TestFinalItemNormalizerReceivesOriginalInput(t *testing.T) {
	p := rawInputProbe{seen: make(chan string, 1)}
	ctx := context.Background()
	turn.Run(ctx, ctx, finalItemModel{}, prompt(), toolList{p}, &recorder{}, turn.Limits{})
	select {
	case got := <-p.seen:
		if got != `{ "value": "x" }` {
			t.Errorf("normalizer input = %q", got)
		}
	default:
		t.Fatal("normalizer not called")
	}
}

type forwardingSource struct{ target turn.Tool }

func (s forwardingSource) Toolset(context.Context, []protocol.LogMessage, []string, string) turn.Toolset {
	return turn.Toolset{Tools: []turn.Tool{forwardingProbe{}}, Deferred: []turn.Tool{s.target}}
}

type forwardingProbe struct{}

func (forwardingProbe) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "forward"} }
func (forwardingProbe) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: "unexpected forwarder execution"}, nil
}
func (forwardingProbe) Forward(c protocol.ToolCall) (protocol.ToolCall, bool) {
	var envelope struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(c.Arguments, &envelope) != nil {
		return c, false
	}
	return protocol.ToolCall{ID: c.ID, Name: "probe", Arguments: envelope.Arguments}, true
}
func (forwardingProbe) ForwardInput(c protocol.ToolCall, input json.RawMessage) (protocol.ToolCall, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(c.Arguments, &envelope); err != nil {
		return c, err
	}
	envelope["arguments"] = input
	b, err := json.Marshal(envelope)
	c.Arguments = b
	return c, err
}

type forwardedNormalizer struct{ seen, ran chan string }

func (forwardedNormalizer) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "probe"} }
func (p forwardedNormalizer) NormalizeInput(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	p.seen <- string(input)
	return json.RawMessage(`{"value":"normalized"}`), nil
}
func (p forwardedNormalizer) Run(_ context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	p.ran <- string(call.Arguments)
	return protocol.ToolResult{Text: "done"}, nil
}

func TestFinalItemNormalizesForwardedInput(t *testing.T) {
	p := forwardedNormalizer{seen: make(chan string, 1), ran: make(chan string, 1)}
	m := &model{replies: []protocol.LogMessage{
		{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartToolCall, CallID: "1", Name: "forward", Arguments: json.RawMessage(`{"action":"call","arguments":{"value":"raw"}}`)}}},
		say("done"),
	}}
	r := &recorder{}
	ctx := context.Background()
	turn.Run(ctx, ctx, m, prompt(), forwardingSource{p}, r, turn.Limits{})
	select {
	case got := <-p.seen:
		if got != `{"value":"raw"}` {
			t.Errorf("normalizer input = %s", got)
		}
	default:
		t.Error("normalizer not called")
	}
	select {
	case got := <-p.ran:
		if got != `{"value":"normalized"}` {
			t.Errorf("executed input = %s", got)
		}
	default:
		t.Error("target not called")
	}
	if len(r.items) == 0 || len(r.items[0].Parts) == 0 {
		t.Fatal("forwarded call not recorded")
	}
	var envelope struct {
		Action    string            `json:"action"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(r.items[0].Parts[0].Arguments, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Action != "call" || envelope.Arguments["value"] != "normalized" {
		t.Errorf("recorded forwarded call = %s", r.items[0].Parts[0].Arguments)
	}
}
