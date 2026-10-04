package backend_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/majorcontext/harness/internal/backend"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
)

type stub struct{ window int }

func (s stub) Capabilities(string) turn.Capabilities {
	return turn.Capabilities{ContextWindow: s.window}
}

func (stub) Run(context.Context, turn.Request, turn.Sink) (turn.Result, error) {
	return turn.Result{}, nil
}

func TestRouterCheck(t *testing.T) {
	known := "codex/" + modelmeta.Models("codex")[0]
	for _, tc := range []struct {
		name   string
		strict bool
		model  string
		want   bool
	}{
		{"a configured provider with a known model", true, known, true},
		{"an unknown model of a configured provider, not strict", false, "codex/no-such-model", true},
		{"an unknown model of a configured provider, strict", true, "codex/no-such-model", false},
		{"a provider that is not configured", false, "nope/model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := backend.NewRouter(map[string]turn.Backend{"codex": stub{}}, tc.strict)
			ref, _ := message.ParseModelRef(tc.model)
			be, err := r.Check(ref)
			if (err == nil) != tc.want || err != nil && !errors.Is(err, backend.ErrUnavailable) || tc.want && be == nil {
				t.Errorf("Check(%q) = %v, %v; want ok = %t, and ErrUnavailable on an error", tc.model, be, err, tc.want)
			}
		})
	}
}

func TestRouterListsTheModelsOfConfiguredProvidersByID(t *testing.T) {
	r := backend.NewRouter(map[string]turn.Backend{"codex": stub{window: 7}, "claude-code": stub{window: 9}}, false)
	var ids []string
	for _, m := range r.List() {
		ids = append(ids, m.ID)
		if want := map[string]int{"codex": 7, "claude-code": 9}[m.Provider]; m.ContextWindow != want {
			t.Errorf("%s window = %d, want %d", m.ID, m.ContextWindow, want)
		}
	}
	if len(ids) == 0 || !slices.IsSorted(ids) {
		t.Errorf("List IDs = %v, want some, sorted", ids)
	}
}
