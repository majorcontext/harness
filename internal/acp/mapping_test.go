package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/acp/rpc"
	"github.com/majorcontext/harness/protocol"
)

func TestRPCErrorMapsEachCode(t *testing.T) {
	rows := []struct {
		err  error
		code int
		text string
	}{
		{harness.ErrInvalidRequest, rpc.CodeInvalidParams, ""},
		{harness.ErrInputConflict, rpc.CodeInvalidParams, ""},
		{harness.ErrRequestNotPending, rpc.CodeInvalidParams, ""},
		{harness.ErrSessionNotFound, codeNotFound, ""},
		{harness.ErrBlobNotFound, codeNotFound, ""},
		{harness.ErrSessionNotOwned, rpc.CodeInternal, ""},
		{harness.ErrOwnershipLost, rpc.CodeInternal, ""},
		{harness.ErrSessionHeld, rpc.CodeInternal, ""},
		{harness.ErrSessionBusy, rpc.CodeInternal, ""},
		{harness.ErrTurnMismatch, rpc.CodeInternal, ""},
		{harness.ErrSessionExists, rpc.CodeInternal, ""},
		{harness.ErrDraining, rpc.CodeInternal, ""},
		{&protocol.Error{Code: protocol.CodeModelUnavailable, Message: "no model"}, rpc.CodeInternal, ""},
		{harness.ErrUnreplayable, rpc.CodeInternal, "Internal error"},
		{&protocol.Error{Code: protocol.CodeInternal, Message: "secret detail"}, rpc.CodeInternal, "Internal error"},
		{errors.New("secret detail"), rpc.CodeInternal, "Internal error"},
	}
	for _, r := range rows {
		got, ok := rpcError(r.err).(*rpc.Error)
		if !ok {
			t.Fatalf("rpcError(%v) is not an *rpc.Error", r.err)
		}
		if got.Code != r.code {
			t.Errorf("rpcError(%v).Code = %d, want %d", r.err, got.Code, r.code)
		}
		if r.text != "" && got.Message != r.text {
			t.Errorf("rpcError(%v).Message = %q, want %q", r.err, got.Message, r.text)
		}
		if data, _ := got.Data.(map[string]any); data["code"] != string(protocol.CodeOf(r.err)) {
			t.Errorf("rpcError(%v).Data = %v, want the protocol code", r.err, got.Data)
		}
	}
}

func TestRPCErrorKeepsTheStopCause(t *testing.T) {
	got := rpcError(harness.ErrOwnershipLost).(*rpc.Error)
	if data, _ := got.Data.(map[string]any); data["cause"] != "ownership lost" {
		t.Errorf("Data = %v, want the cause ownership lost", got.Data)
	}
}

func TestSessionServersConvertsBothTransports(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"name":"a","command":"/bin/a","args":["-x"],"env":[{"name":"K","value":"V"}]}`),
		json.RawMessage(`{"type":"http","name":"b","url":"https://h/mcp","headers":[{"name":"Authorization","value":"t"}]}`),
	}
	got, err := sessionServers(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a := got["a"]; !slices.Equal(a.Command, []string{"/bin/a", "-x"}) || !slices.Equal(a.Env, []string{"K=V"}) {
		t.Errorf("stdio server = %+v", a)
	}
	if b := got["b"]; b.URL != "https://h/mcp" || b.Headers["Authorization"] != "t" {
		t.Errorf("http server = %+v", b)
	}
}

func TestSessionServersRefusesWhatItCannotRun(t *testing.T) {
	for name, raw := range map[string]string{
		"a repeated name":    `{"name":"a","command":"/bin/a","args":[],"env":[]}`,
		"an sse server":      `{"type":"sse","name":"s","url":"https://h","headers":[]}`,
		"a nameless server":  `{"command":"/bin/a","args":[],"env":[]}`,
		"a repeated header":  `{"type":"http","name":"h","url":"https://h","headers":[{"name":"X","value":"1"},{"name":"X","value":"2"}]}`,
		"a stdio no command": `{"name":"c","args":[],"env":[]}`,
	} {
		list := []json.RawMessage{json.RawMessage(`{"name":"a","command":"/bin/a","args":[],"env":[]}`), json.RawMessage(raw)}
		_, err := sessionServers(list)
		if e, ok := err.(*rpc.Error); !ok || e.Code != rpc.CodeInvalidParams {
			t.Errorf("%s: err = %v, want -32602", name, err)
		}
	}
}

func TestPromptPartsTakeTheBaselineBlocks(t *testing.T) {
	parts, err := promptParts([]json.RawMessage{
		json.RawMessage(`{"type":"text","text":"hello"}`),
		json.RawMessage(`{"type":"resource_link","name":"doc","uri":"file:///doc"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Text != "hello" || parts[1].Text != "doc (file:///doc)" {
		t.Errorf("parts = %+v", parts)
	}
	for _, bad := range []string{`{"type":"image","data":"","mimeType":"image/png"}`, `[]`, `7`} {
		if _, err := promptParts([]json.RawMessage{json.RawMessage(bad)}); err == nil {
			t.Errorf("promptParts accepted %s", bad)
		}
	}
	if _, err := promptParts(nil); err == nil {
		t.Error("promptParts accepted no block")
	}
}

func TestNewULIDHasTheULIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	seen := map[string]bool{}
	for range 100 {
		id := newULID()
		if !re.MatchString(id) || seen[id] {
			t.Fatalf("newULID() = %q: bad shape or repeated (%s)", id, fmt.Sprint(len(seen)))
		}
		seen[id] = true
	}
}

func TestTailSendsOnlyTheUnsentSuffix(t *testing.T) {
	rows := []struct {
		name, sent, full, text string
		resend                 bool
	}{
		{"nothing sent", "", "hello", "hello", false},
		{"prefix sent", "hel", "hello", "lo", false},
		{"all sent", "hello", "hello", "", false},
		{"multi-byte rune before the cut", "a\u20ac", "a\u20acb\u20acc", "b\u20acc", false},
		{"a dropped frame leaves a hole", "ab", "a\u20acb", "a\u20acb", true},
		{"more sent than the item holds", "hello!", "hello", "hello", true},
		{"sent ends inside a rune", "a\xe2\x82", "a\u20acb", "a\u20acb", true},
	}
	for _, r := range rows {
		text, resend := tailOf(r.sent, r.full)
		if text != r.text || resend != r.resend {
			t.Errorf("%s: tailOf(%q, %q) = %q, %v; want %q, %v", r.name, r.sent, r.full, text, resend, r.text, r.resend)
		}
	}
}

func TestSessionServersTakeAnExplicitStdioType(t *testing.T) {
	got, err := sessionServers([]json.RawMessage{json.RawMessage(`{"type":"stdio","name":"a","command":"/bin/a","args":[],"env":[]}`)})
	if err != nil || !slices.Equal(got["a"].Command, []string{"/bin/a"}) {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestPromptResultAfterACancelIsCancelled(t *testing.T) {
	failed := harness.TurnResult{StopReason: protocol.StopFailed, Error: "boom", ErrorClass: protocol.ErrorPermanent}
	got, err := promptResult(failed, true)
	if err != nil || got.(map[string]string)["stopReason"] != "cancelled" {
		t.Errorf("a failed turn after a cancel = %v, %v; want cancelled", got, err)
	}
	if _, err := promptResult(failed, false); err == nil {
		t.Error("a failed turn without a cancel must be an error")
	}
	if got := waitFailure(errors.New("model closed"), true); got != nil {
		t.Errorf("waitFailure after a cancel = %v, want nil", got)
	}
	if got := waitFailure(harness.ErrOwnershipLost, true); got == nil {
		t.Error("lost ownership after a cancel must stay an error")
	}
}

type noRuntimes struct{ calls int }

func (n *noRuntimes) For(context.Context, string) (Runtime, error) {
	n.calls++
	return nil, errors.New("unexpected")
}
func (n *noRuntimes) Close(context.Context) error { return nil }

func TestSessionNewAfterShutdownIsRefusedWithoutBuildingARuntime(t *testing.T) {
	rts := &noRuntimes{}
	a := &agent{rts: rts, base: t.Context(), sessions: map[string]*held{}}
	a.EndOfInput()
	_, err := a.newSession(t.Context(), json.RawMessage(`{"cwd":"/","mcpServers":[]}`))
	e, ok := err.(*rpc.Error)
	if !ok || e.Code != rpc.CodeInternal || e.Data.(map[string]any)["code"] != "session_not_owned" {
		t.Errorf("err = %v, want -32603 session_not_owned", err)
	}
	if rts.calls != 0 || len(a.sessions) != 0 {
		t.Errorf("For calls = %d, sessions = %d, want none", rts.calls, len(a.sessions))
	}
}

func TestSessionServersTreatHeaderNamesAsCaseInsensitive(t *testing.T) {
	_, err := sessionServers([]json.RawMessage{json.RawMessage(`{"type":"http","name":"h","url":"https://h","headers":[{"name":"Authorization","value":"1"},{"name":"authorization","value":"2"}]}`)})
	if e, ok := err.(*rpc.Error); !ok || e.Code != rpc.CodeInvalidParams {
		t.Errorf("err = %v, want -32602", err)
	}
	got, err := sessionServers([]json.RawMessage{json.RawMessage(`{"type":"http","name":"h","url":"https://h","headers":[{"name":"x-token","value":"1"}]}`)})
	if err != nil || got["h"].Headers["X-Token"] != "1" {
		t.Errorf("headers = %+v, %v; want the canonical name X-Token", got["h"].Headers, err)
	}
}

func TestRequiredFieldsMustBePresent(t *testing.T) {
	for _, raw := range []string{
		`{"name":"a","command":"/bin/a","env":[]}`,
		`{"name":"a","command":"/bin/a","args":null,"env":[]}`,
		`{"name":"a","command":"/bin/a","args":[]}`,
		`{"name":"a","command":"/bin/a","args":[],"env":[{"name":"K"}]}`,
		`{"type":"http","name":"h","url":"https://h"}`,
		`{"type":"http","name":"h","url":"https://h","headers":[{"value":"v"}]}`,
	} {
		if _, err := sessionServers([]json.RawMessage{json.RawMessage(raw)}); err == nil {
			t.Errorf("sessionServers accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"type":"text"}`, `{"type":"text","text":null}`, `{"type":"resource_link","uri":"u"}`, `{"type":"resource_link","name":"n"}`, `{"text":"x"}`} {
		if _, err := promptParts([]json.RawMessage{json.RawMessage(raw)}); err == nil {
			t.Errorf("promptParts accepted %s", raw)
		}
	}
	if _, err := promptParts([]json.RawMessage{json.RawMessage(`{"type":"text","text":""}`)}); err != nil {
		t.Errorf("an empty text is present: %v", err)
	}
}

func TestAnswerAfterACancelledRequestIsTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := answer(ctx, harness.TurnResult{StopReason: protocol.StopCompleted}, false); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got, err := answer(t.Context(), harness.TurnResult{StopReason: protocol.StopCompleted}, false); err != nil || got.(map[string]string)["stopReason"] != "end_turn" {
		t.Errorf("answer = %v, %v", got, err)
	}
}
