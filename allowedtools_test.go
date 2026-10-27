package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestOpenReplacesAllowedToolsBeforeTheNextTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created []string
		opened  []string
		repeat  []string
		tools   []string
		changes int
	}{
		{"widen", []string{"echo"}, []string{"echo", "secret"}, []string{"secret", "echo", "echo"}, []string{"echo", "secret"}, 1},
		{"narrow", []string{"echo", "secret"}, []string{"secret"}, []string{"secret"}, []string{"secret"}, 1},
		{"allow none", nil, []string{}, []string{}, nil, 1},
		{"allow all", []string{}, nil, nil, []string{"echo", "secret"}, 1},
		{"same set", []string{"echo", "secret"}, []string{"secret", "echo", "echo"}, []string{"echo", "secret"}, []string{"echo", "secret"}, 0},
		{"same nil", nil, nil, nil, []string{"echo", "secret"}, 0},
		{"same empty", []string{}, []string{}, []string{}, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, harnesstest.Step{
				Name: "done", Match: harnesstest.LastUserText("hi"), Repeat: true, Reply: harnesstest.Reply{Text: "done"}})
			st := store.NewMemStore()
			r := allowlistRuntime(t, st, server)
			if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "test/gpt-5", AllowedTools: tc.created}); err != nil {
				t.Fatal(err)
			}
			closeRuntime(t, r)
			r = allowlistRuntime(t, st, server)
			if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "test/gpt-5", AllowedTools: tc.created}); !errors.Is(err, harness.ErrSessionExists) {
				t.Fatalf("Create existing = %v", err)
			}
			names := slices.Clone(tc.opened)
			option := harness.WithAllowedTools(names)
			if len(names) > 0 {
				names[0] = "changed after option creation"
			}
			s, err := r.Open(t.Context(), "s1", option)
			if err != nil {
				t.Fatal(err)
			}
			assertAllowlistTurn(t, s, server, tc.opened, tc.tools)
			head := s.Snapshot().HeadSeq
			if same, err := r.Open(t.Context(), "s1", harness.WithAllowedTools(tc.created)); err != nil || same != s || s.Snapshot().HeadSeq != head {
				t.Fatalf("Open running session = %v, %v; head = %d, want %d", same, err, s.Snapshot().HeadSeq, head)
			}
			closeRuntime(t, r)
			r = allowlistRuntime(t, st, server)
			if _, err := r.Open(t.Context(), "s1", harness.WithAllowedTools(tc.repeat)); err != nil {
				t.Fatal(err)
			}
			closeRuntime(t, r)
			r = allowlistRuntime(t, st, server)
			s, err = r.Open(t.Context(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			assertAllowlistTurn(t, s, server, tc.opened, tc.tools)
			closeRuntime(t, r)
			view, err := harness.OpenView(t.Context(), st, "s1")
			if err != nil {
				t.Fatal(err)
			}
			assertAllowlistSnapshot(t, view.Snapshot(), tc.opened)
			changes := allowlistChanges(t, st)
			if len(changes) != tc.changes {
				t.Fatalf("allowlist changes = %v, want %d", changes, tc.changes)
			}
			if len(changes) > 0 && !reflect.DeepEqual(changes[0], tc.opened) {
				t.Fatalf("durable allowlist = %#v, want %#v", changes[0], tc.opened)
			}
		})
	}
}

func TestOpenValidatesTheReplacementAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		updated string
		created []string
		opened  []string
		shadow  bool
		reject  bool
	}{
		{"unknown tool at replayed model", "openai/gpt-5", "claude-code/sonnet", []string{}, []string{"unknown"}, false, true},
		{"shadowed builtin", "openai/gpt-5", "", nil, nil, true, true},
		{"shadowed replacement leaves log unchanged", "openai/gpt-5", "", []string{}, nil, true, true},
		{"explicit replacement of builtin", "openai/gpt-5", "", nil, []string{"read_file"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemStore()
			opts := harness.Options{Store: st, Config: config.Config{Providers: map[string]config.Provider{"openai": {Auth: config.AuthInjected}}}}
			r, err := harness.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: tc.model, AllowedTools: tc.created})
			if err != nil {
				t.Fatal(err)
			}
			if tc.updated != "" {
				if _, err := s.Update(t.Context(), protocol.SettingsPatch{Model: &tc.updated}); err != nil {
					t.Fatal(err)
				}
			}
			closeRuntime(t, r)
			if tc.shadow {
				opts.WorkDir, opts.Tools = t.TempDir(), []harness.Tool{newProbe("read_file", false)}
			}
			r, err = harness.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, r) })
			before := rawLog(t, st)
			s, err = r.Open(t.Context(), "s1", harness.WithAllowedTools(tc.opened))
			if tc.reject {
				if tc.shadow && !reflect.DeepEqual(before, rawLog(t, st)) {
					t.Error("rejected Open changed the log")
				}
				if !errors.Is(err, harness.ErrInvalidRequest) || len(allowlistChanges(t, st)) != 0 {
					t.Fatalf("Open = %v; allowlist changes = %v", err, allowlistChanges(t, st))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertAllowlistSnapshot(t, s.Snapshot(), tc.opened)
			closeRuntime(t, r)
			r, err = harness.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			s, err = r.Open(t.Context(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			assertAllowlistSnapshot(t, s.Snapshot(), tc.opened)
		})
	}
}

func TestOpenKeepsEqualAllowlistsWithoutModelValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created []string
		opened  []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{"same set", []string{"echo", "secret"}, []string{"secret", "echo", "echo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{})
			st := store.NewMemStore()
			r := allowlistRuntime(t, st, server)
			if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "test/gpt-5", AllowedTools: tc.created}); err != nil {
				t.Fatal(err)
			}
			closeRuntime(t, r)
			for _, options := range [][]harness.SessionOption{nil, {harness.WithAllowedTools(tc.opened)}} {
				r, err := harness.New(harness.Options{Store: st})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { closeRuntime(t, r) })
				s, err := r.Open(t.Context(), "s1", options...)
				if err != nil {
					t.Fatal(err)
				}
				assertAllowlistSnapshot(t, s.Snapshot(), tc.created)
				closeRuntime(t, r)
			}
			if changes := allowlistChanges(t, st); len(changes) != 0 {
				t.Fatalf("equal allowlist appended changes: %v", changes)
			}
		})
	}
}

func TestCreateRejectsOpenOnlyOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		option harness.SessionOption
	}{
		{"allowed tools", harness.WithAllowedTools(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemStore()
			r, err := harness.New(harness.Options{Store: st})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, r) })
			if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1"}, tc.option); !errors.Is(err, harness.ErrInvalidRequest) {
				t.Fatalf("Create with Open-only option = %v", err)
			}
			if got := rawLog(t, st); len(got) != 0 {
				t.Fatalf("rejected Create wrote records: %v", got)
			}
		})
	}
}

func allowlistRuntime(t *testing.T, st harness.Store, server *harnesstest.OpenAI) *harness.Runtime {
	t.Helper()
	r, err := harness.New(harness.Options{Store: st, Tools: []harness.Tool{newProbe("echo", false), newProbe("secret", false)},
		Getenv: func(string) string { return "k" }, Config: config.Config{RequestMaxRetries: new(0), StreamMaxRetries: new(0), Providers: map[string]config.Provider{
			"test": {Type: config.TypeOpenAI, APIKeyEnv: "TEST_KEY", BaseURL: server.URL() + "/backend-api/codex", ResponsesPath: "/responses"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r
}

func assertAllowlistTurn(t *testing.T, s *harness.Session, server *harnesstest.OpenAI, allowed, want []string) {
	t.Helper()
	assertAllowlistSnapshot(t, s.Snapshot(), allowed)
	turn, err := s.Prompt(t.Context(), protocol.Input{ID: fmt.Sprint(s.Snapshot().HeadSeq), Parts: []protocol.Part{{Type: protocol.PartText, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(t.Context())
	if err != nil || result.StopReason != protocol.StopCompleted {
		t.Fatalf("turn = %+v, %v", result, err)
	}
	for _, req := range server.Requests() {
		if !slices.Equal(req.Tools, want) {
			t.Errorf("request tools = %q, want %q", req.Tools, want)
		}
	}
}

func assertAllowlistSnapshot(t *testing.T, snapshot protocol.Session, want []string) {
	t.Helper()
	got := snapshot.AllowedTools
	if (got == nil) != (want == nil) || !slices.Equal(slices.Compact(slices.Sorted(slices.Values(got))), slices.Compact(slices.Sorted(slices.Values(want)))) {
		t.Fatalf("snapshot allowlist = %#v, want %#v", got, want)
	}
}

func allowlistChanges(t *testing.T, st harness.Store) [][]string {
	t.Helper()
	var out [][]string
	for ev, err := range harness.ReadEvents(context.Background(), st, "s1", 0) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != protocol.KindSettingsChanged {
			continue
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if raw, ok := data["allowed_tools"]; ok {
			var names []string
			if err := json.Unmarshal(raw, &names); err != nil {
				t.Fatal(err)
			}
			out = append(out, names)
		}
	}
	return out
}

func TestOpenSetsTheStepLimitBeforeTheNextTurn(t *testing.T) {
	server := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, harnesstest.Step{
		Name: "loop", Match: harnesstest.LastUserText("hi"), Repeat: true, Reply: harnesstest.Reply{
			ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "echo", Input: map[string]any{}}}}})
	st := store.NewMemStore()
	r := allowlistRuntime(t, st, server)
	if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "test/gpt-5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s2", Model: "test/gpt-5"}, harness.WithMaxSteps(1)); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("Create with WithMaxSteps = %v", err)
	}
	closeRuntime(t, r)
	r = allowlistRuntime(t, st, server)
	if _, err := r.Open(t.Context(), "s1", harness.WithMaxSteps(-1)); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("Open with a negative limit = %v", err)
	}
	s, err := r.Open(t.Context(), "s1", harness.WithMaxSteps(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().MaxSteps; got != 1 {
		t.Fatalf("snapshot max_steps = %d, want 1", got)
	}
	turn, err := s.Prompt(t.Context(), protocol.Input{ID: "i1", Parts: []protocol.Part{{Type: protocol.PartText, Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := turn.Wait(t.Context()); err != nil || result.StopReason != protocol.StopStepLimit {
		t.Fatalf("turn = %+v, %v", result, err)
	}
	if n := len(server.Requests()); n != 1 {
		t.Fatalf("model requests = %d, want 1", n)
	}
	closeRuntime(t, r)
	for _, opts := range [][]harness.SessionOption{{harness.WithMaxSteps(1)}, nil} {
		r = allowlistRuntime(t, st, server)
		if s, err = r.Open(t.Context(), "s1", opts...); err != nil || s.Snapshot().MaxSteps != 1 {
			t.Fatalf("reopen = %v, %v", s, err)
		}
		closeRuntime(t, r)
	}
	if got := maxStepsChanges(t, st); !slices.Equal(got, []int{1}) {
		t.Fatalf("max_steps changes = %v, want [1]", got)
	}
}

func maxStepsChanges(t *testing.T, st harness.Store) []int {
	t.Helper()
	var out []int
	for ev, err := range harness.ReadEvents(context.Background(), st, "s1", 0) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind != protocol.KindSettingsChanged {
			continue
		}
		var data struct {
			MaxSteps *int `json:"max_steps"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.MaxSteps != nil {
			out = append(out, *data.MaxSteps)
		}
	}
	return out
}
