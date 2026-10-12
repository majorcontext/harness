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
	"github.com/majorcontext/harness/protocol"
)

func TestContractRuntimeOptions(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a_runtime_with_no_getenv_reads_no_api_key_variable", rowARuntimeWithNoGetenvReadsNoAPIKeyVariable},
		{"a_runtime_with_a_getenv_sends_the_key_that_it_returns", rowARuntimeWithAGetenvSendsTheKeyThatItReturns},
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
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Getenv: getenv, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_OPTIONS_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
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

func rowTheLoggerOptionReceivesTheLogLinesOfTheRuntime(t *testing.T) {
	st := harness.NewMemStore()
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
	h := harness.ReadHandler(failingStore{harness.NewMemStore(), errors.New("store down: disk full")},
		harness.ReadOptions{Logger: slog.New(slog.NewTextHandler(&buf, nil))})
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
	r := lifetimeRuntimeOn(t, harness.NewMemStore(), nil)
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
