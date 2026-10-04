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
// create and on wake, before any turn starts, and ends with the actor. A
// failed warm-up costs only the speed of the first turn, so its error is
// dropped.
func (a *Actor) warm() {
	if _, ok := a.cfg.Backend.(turn.Warmer); !ok {
		return
	}
	req := turn.Request{SessionID: a.cfg.ID, Model: a.state.Model(), Settings: a.state.Settings(), Instructions: a.cfg.Prompt(a.state.Agent()),
		History: a.state.History(), AllowedTools: a.state.AllowedTools()}
	tools, src := a.turnTools(&running{ownsLoop: a.cfg.Backend.Capabilities(req.Model).OwnsLoop})
	warming := make(chan struct{})
	a.warming = warming
	a.cfg.Go(func() {
		defer close(warming)
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

// awaitWarm waits for the warm-up in flight, so a turn never dials the
// transport while the warm-up is about to hold it. A warm-up ends within
// warmTimeout or with the actor.
func (a *Actor) awaitWarm(ctx context.Context) {
	if a.warming == nil {
		return
	}
	select {
	case <-a.warming:
	case <-ctx.Done():
	}
}
