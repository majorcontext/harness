package session

import (
	"context"

	"github.com/majorcontext/harness/internal/eventlog"
)

// Update appends the settings in ch that differ from the session settings.
// Config.Check validates a new model first.
func (a *Actor) Update(ctx context.Context, ch eventlog.SettingsChanged) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		set, prev := a.state.Settings(), a.state.Model()
		ch.Model = changed(ch.Model, prev)
		ch.Effort = changed(ch.Effort, set.Effort)
		ch.ServiceTier = changed(ch.ServiceTier, set.ServiceTier)
		var err error
		if ch.Model != nil && a.cfg.Check != nil {
			err = a.cfg.Check(prev, *ch.Model, a.state.AllowedTools())
		}
		if err == nil && ch != (eventlog.SettingsChanged{}) {
			var events []eventlog.Event
			if ch.Model != nil && eventlog.ProviderOf(*ch.Model) != eventlog.ProviderOf(prev) {
				events = a.dismissRequests()
			}
			err = a.append(append(events, ch)...)
			if err == nil && ch.Model != nil && a.cfg.Backend.Capabilities(*ch.Model).ContextWindow != a.cfg.Backend.Capabilities(prev).ContextWindow {
				a.latched = false
			}
		}
		reply(struct{}{}, err)
	})
	return err
}

func changed(v *string, current string) *string {
	if v == nil || *v == current {
		return nil
	}
	return v
}
