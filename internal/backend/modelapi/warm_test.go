package modelapi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/provider"
)

type warmClient struct {
	provider.Provider
	enabled bool
	got     []*provider.Request
}

func (c *warmClient) StartupPrewarmEnabled() bool { return c.enabled }
func (c *warmClient) Warm(_ context.Context, r *provider.Request) error {
	c.got = append(c.got, r)
	return nil
}

func TestWarmReachesOnlyAClientThatCanWarm(t *testing.T) {
	req := turn.Request{SessionID: "s1", Model: "codex/gpt-5", Instructions: "be brief"}
	off, on, bare := &warmClient{}, &warmClient{enabled: true}, struct{ provider.Provider }{}
	b := []*modelapi.Backend{modelapi.New(off, 0), modelapi.New(on, 0), modelapi.New(bare, 0)}
	if err := errors.Join(b[0].Warm(context.Background(), req), b[1].Warm(context.Background(), req), b[2].Warm(context.Background(), req)); err != nil {
		t.Fatal(err)
	}
	if m := req.Model; b[0].CanWarm(m) || !b[1].CanWarm(m) || b[2].CanWarm(m) || len(off.got) != 0 || len(on.got) != 1 || on.got[0].System[0] != "be brief" {
		t.Errorf("CanWarm is wrong or the warm requests are: off %v, on %+v", off.got, on.got)
	}
}
