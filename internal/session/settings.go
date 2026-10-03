package session

import (
	"context"

	"github.com/majorcontext/harness/internal/eventlog"
)

// Update appends the settings in ch that differ from the session settings.
// Config.Check validates a new model first.
func (a *Actor) Update(ctx context.Context, ch eventlog.SettingsChanged) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		set := a.state.Settings()
		ch.Model = changed(ch.Model, a.state.Model())
		ch.Effort = changed(ch.Effort, set.Effort)
		ch.ServiceTier = changed(ch.ServiceTier, set.ServiceTier)
		var err error
		if ch.Model != nil && a.cfg.Check != nil {
			err = a.cfg.Check(*ch.Model, a.state.AllowedTools())
		}
		if err == nil && ch != (eventlog.SettingsChanged{}) {
			err = a.append(ch)
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
