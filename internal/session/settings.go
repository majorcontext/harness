package session

import (
	"context"
	"maps"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/eventlog"
)

// Update appends settings and title changes after validation.
func (a *Actor) Update(ctx context.Context, ch protocol.SettingsChanged, title *string) error {
	return a.update(ctx, ch, title, false)
}

// UpdateAccountsIfIdle sets account selections only when no turn is active.
func (a *Actor) UpdateAccountsIfIdle(ctx context.Context, accounts map[string]string) error {
	return a.update(ctx, protocol.SettingsChanged{Accounts: &accounts}, nil, true)
}

func (a *Actor) update(ctx context.Context, ch protocol.SettingsChanged, title *string, accountsIdle bool) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		if accountsIdle && a.run != nil {
			reply(struct{}{}, ErrBusy)
			return
		}
		n := len(a.state.Requests())
		err := a.applySettings(a.cfg.Base, ch, title)
		if err == nil && n > len(a.state.Requests()) && a.run == nil {
			err = a.next(true)
		}
		reply(struct{}{}, err)
	})
	return err
}

func (a *Actor) applySettings(ctx context.Context, ch protocol.SettingsChanged, title *string, before ...protocol.Payload) error {
	set, prev := a.state.Settings(), a.state.Model()
	ch.Model = changed(ch.Model, prev)
	ch.Effort = changed(ch.Effort, set.Effort)
	ch.ServiceTier = changed(ch.ServiceTier, set.ServiceTier)
	if ch.Accounts != nil && maps.Equal(*ch.Accounts, set.Accounts) {
		ch.Accounts = nil
	}
	hasSettings := ch.Model != nil || ch.Effort != nil || ch.ServiceTier != nil || ch.Accounts != nil
	if ch.Model != nil && a.cfg.Check != nil {
		if err := a.cfg.Check(prev, *ch.Model, a.state.AllowedTools()); err != nil {
			return err
		}
	}
	title = changed(title, a.state.Renamed())
	if !hasSettings && title == nil {
		if len(before) == 0 {
			return nil
		}
		return a.appendCtx(ctx, before...)
	}
	events := before
	if ch.Model != nil && eventlog.ProviderOf(*ch.Model) != eventlog.ProviderOf(prev) {
		events = append(events, a.dismissRequests()...)
	}
	if hasSettings {
		events = append(events, ch)
	}
	if title != nil {
		events = append(events, protocol.SessionTitled{Title: *title, Source: protocol.TitleUser})
	}
	if err := a.appendCtx(ctx, events...); err != nil {
		return err
	}
	if ch.Model != nil && a.cfg.Backend.Capabilities(*ch.Model).ContextWindow != a.cfg.Backend.Capabilities(prev).ContextWindow {
		a.latched = false
	}
	return nil
}

// DeferModel validates ref and replaces the model change of the running turn.
func (a *Actor) DeferModel(ctx context.Context, ref string) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		if a.run == nil || a.run.kind != kindTurn || !a.run.ownsLoop {
			reply(struct{}{}, ErrBusy)
			return
		}
		if a.cfg.Check != nil {
			if err := a.cfg.Check(a.state.Model(), ref, a.state.AllowedTools()); err != nil {
				reply(struct{}{}, err)
				return
			}
		}
		a.pendingModel = ref
		if ref == a.state.Model() {
			a.pendingModel = ""
		}
		reply(struct{}{}, nil)
	})
	return err
}

// PendingModel returns the model change that waits for the end of the turn.
func (a *Actor) PendingModel(ctx context.Context) (string, error) {
	return call(ctx, a, func(reply func(string, error)) { reply(a.pendingModel, nil) })
}

func changed(v *string, current string) *string {
	if v == nil || *v == current {
		return nil
	}
	return v
}
