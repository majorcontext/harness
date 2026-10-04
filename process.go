package harness

import (
	"context"

	"github.com/majorcontext/harness/process"
)

// processRoutes are the processes of the HTTP API. A start, a stop, or a
// restart is work that Close waits for, and fails with ErrDraining after
// Close starts, so Close stops every process that a route starts.
type processRoutes struct{ r *Runtime }

func (p processRoutes) List() []process.Info { return p.r.procs.List() }

func (p processRoutes) Logs(name string, tail int) (string, process.Status, error) {
	return p.r.procs.Logs(name, tail)
}

func (p processRoutes) Start(ctx context.Context, name string) (process.Status, error) {
	return p.held(ctx, name, (*process.Manager).Start)
}

func (p processRoutes) Stop(ctx context.Context, name string) (process.Status, error) {
	return p.held(ctx, name, (*process.Manager).Stop)
}

func (p processRoutes) Restart(ctx context.Context, name string) (process.Status, error) {
	return p.held(ctx, name, (*process.Manager).Restart)
}

// held runs action as work that Close waits for. A Close whose ctx ends
// ends the wait for the ready gate.
func (p processRoutes) held(ctx context.Context, name string, action func(*process.Manager, context.Context, string) (process.Status, error)) (process.Status, error) {
	if err := p.r.hold(); err != nil {
		return process.Status{}, err
	}
	defer p.r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(p.r.base, cancel)()
	return action(p.r.procs, ctx, name)
}
