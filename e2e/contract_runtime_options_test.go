package e2e

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/httpapi"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestContractRuntimeOptions(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a_runtime_with_no_getenv_reads_no_api_key_variable", rowARuntimeWithNoGetenvReadsNoAPIKeyVariable},
		{"a_runtime_with_a_getenv_sends_the_key_that_it_returns", rowARuntimeWithAGetenvSendsTheKeyThatItReturns},
		{"a_turn_with_no_api_key_says_where_the_key_was_read", rowATurnWithNoAPIKeySaysWhereTheKeyWasRead},
		{"the_logger_option_receives_the_log_lines_of_the_runtime", rowTheLoggerOptionReceivesTheLogLinesOfTheRuntime},
		{"the_read_handler_logs_the_cause_of_an_error_that_it_masks", rowTheReadHandlerLogsTheCauseOfAnErrorThatItMasks},
		{"commands_returns_the_cause_of_an_ended_context", rowCommandsReturnsTheCauseOfAnEndedContext},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.run(t)
		})
	}
}

func authorizationOfOneTurn(t *testing.T, getenv func(string) string, steps ...harnesstest.Step) string {
	t.Helper()
	fake := harnesstest.NewChat(t, steps...)
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: getenv, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_OPTIONS_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	reqs := fake.Requests()
	if len(reqs) == 0 {
		return ""
	}
	return reqs[0].Header.Get("Authorization")
}

var reply = harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "ok"}}

func rowARuntimeWithNoGetenvReadsNoAPIKeyVariable(t *testing.T) {
	t.Setenv("HARNESS_E2E_OPTIONS_KEY", "from-the-process")
	if got := authorizationOfOneTurn(t, nil); got != "" {
		t.Errorf("Authorization = %q, want no key: the variable of the process is unread", got)
	}
}

func rowARuntimeWithAGetenvSendsTheKeyThatItReturns(t *testing.T) {
	t.Setenv("HARNESS_E2E_OPTIONS_KEY", "from-the-process")
	if got := authorizationOfOneTurn(t, os.Getenv, reply); got != "Bearer from-the-process" {
		t.Errorf("Authorization = %q, want the key of os.Getenv", got)
	}
	got := authorizationOfOneTurn(t, func(key string) string {
		if key != "HARNESS_E2E_OPTIONS_KEY" {
			return ""
		}
		return "from-the-embedder"
	}, reply)
	if got != "Bearer from-the-embedder" {
		t.Errorf("Authorization = %q, want the key of the embedder", got)
	}
}

func errorOfOneTurn(t *testing.T, getenv func(string) string, providers map[string]config.Provider, model string) string {
	t.Helper()
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: getenv, Config: config.Config{ContextWindowTokens: 100000, Providers: providers}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: model})
	if err != nil {
		return err.Error()
	}
	turn, err := s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := turn.Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != protocol.StopFailed {
		t.Fatalf("StopReason = %q, want failed", res.StopReason)
	}
	return res.Error
}

func rowATurnWithNoAPIKeySaysWhereTheKeyWasRead(t *testing.T) {
	t.Setenv("HARNESS_E2E_OPTIONS_KEY", "from-the-process")
	t.Setenv("ANTHROPIC_API_KEY", "from-the-process")
	compat := func(env string) map[string]config.Provider {
		return map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1", APIKeyEnv: env}}
	}
	empty := func(string) string { return "" }
	for _, c := range []struct {
		name      string
		getenv    func(string) string
		providers map[string]config.Provider
		model     string
		want      string
	}{
		{"compat_nil_getenv", nil, compat("HARNESS_E2E_OPTIONS_KEY"), "bifrost/gpt-test",
			"openaicompat(bifrost): no API key: Options.Getenv is nil, so HARNESS_E2E_OPTIONS_KEY is unread"},
		{"compat_empty_variable", empty, compat("HARNESS_E2E_OPTIONS_KEY"), "bifrost/gpt-test",
			"openaicompat(bifrost): no API key: HARNESS_E2E_OPTIONS_KEY is empty"},
		{"compat_no_api_key_env", os.Getenv, compat(""), "bifrost/gpt-test",
			"openaicompat(bifrost): no API key: the providers entry sets no api_key_env"},
		{"anthropic_nil_getenv", nil, nil, "anthropic/claude-fable-5",
			"harness: model unavailable: provider \"anthropic\" is unavailable for anthropic/claude-fable-5: Options.Getenv is nil, so ANTHROPIC_API_KEY is unread"},
		{"openai_empty_variable", empty, nil, "openai/gpt-5",
			"harness: model unavailable: provider \"openai\" is unavailable for openai/gpt-5: OPENAI_API_KEY is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := errorOfOneTurn(t, c.getenv, c.providers, c.model); got != c.want {
				t.Errorf("turn error = %q, want %q", got, c.want)
			}
		})
	}
}

func rowTheLoggerOptionReceivesTheLogLinesOfTheRuntime(t *testing.T) {
	st := store.NewMemStore()
	if err := st.Append(t.Context(), "bad", 0, []byte("not an envelope")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	r, err := harness.New(harness.Options{Store: st, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	if _, err := r.List(t.Context(), protocol.ListSessions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "session skipped in the list") {
		t.Errorf("logger output = %q, want the list warning", buf.String())
	}
}

type failingStore struct {
	harness.Store
	err error
}

func (s failingStore) Head(context.Context, string) (uint64, error) { return 0, s.err }

func rowTheReadHandlerLogsTheCauseOfAnErrorThatItMasks(t *testing.T) {
	var buf bytes.Buffer
	h := httpapi.ReadHandler(failingStore{store.NewMemStore(), errors.New("store down: disk full")},
		httpapi.ReadOptions{Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("response = %d %q, want 500 with the cause masked", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), "disk full") {
		t.Errorf("logger output = %q, want the cause", buf.String())
	}
}

func rowCommandsReturnsTheCauseOfAnEndedContext(t *testing.T) {
	r := lifetimeRuntimeOn(t, store.NewMemStore(), nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	boom := errors.New("caller left")
	cancel(boom)
	if _, err := r.Commands(ctx); !errors.Is(err, boom) {
		t.Errorf("Commands = %v, want the cause of the context", err)
	}
	if menu, err := r.Commands(t.Context()); err != nil || len(menu.Commands) == 0 {
		t.Errorf("Commands = %d commands, %v; want the menu", len(menu.Commands), err)
	}
}
