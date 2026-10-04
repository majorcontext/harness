package session

import (
	"context"
	"time"

	"github.com/majorcontext/harness/internal/turn"
)

// warmTimeout bounds a warm-up.
const warmTimeout = 15 * time.Second

// warm asks the backend to prepare its transport for the first turn, from a
// goroutine under the session context. It runs once when the actor runs, on
// create and on wake, and ends with the actor. A failed warm-up costs only
// the speed of the first turn, so its error is dropped.
func (a *Actor) warm() {
	if _, ok := a.cfg.Backend.(turn.Warmer); !ok {
		return
	}
	req := turn.Request{SessionID: a.cfg.ID, Model: a.state.Model(), Settings: a.state.Settings(), Instructions: a.cfg.Prompt(a.state.Agent()),
		History: a.state.History(), AllowedTools: a.state.AllowedTools()}
	tools, src := a.turnTools("", a.cfg.Backend.Capabilities(req.Model).OwnsLoop)
	a.cfg.Go(func() {
		ctx, cancel := context.WithTimeout(a.cfg.Base, warmTimeout)
		defer cancel()
		go func() {
			select {
			case <-a.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		_ = turn.Warm(ctx, a.cfg.Backend, req, tools, src)
	})
}
