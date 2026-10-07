package harness

import (
	"context"

	"github.com/majorcontext/harness/internal/process"
	"github.com/majorcontext/harness/protocol"
)

// processRoutes are the processes of the HTTP API. A start, a stop, or a
// restart is work that Close waits for, and fails with ErrDraining after
// Close starts, so Close stops every process that a route starts.
type processRoutes struct{ r *Runtime }

func (p processRoutes) List() []protocol.ProcessInfo {
	list := p.r.procs.List()
	out := make([]protocol.ProcessInfo, len(list))
	for i, in := range list {
		out[i] = protocol.ProcessInfo{Name: in.Name, Origin: string(in.Origin), Command: in.Command, Dir: in.Dir, EnvNames: in.EnvNames,
			Ports: in.Ports, ReadyRegex: in.ReadyRegex, ReadyPort: in.ReadyPort, ReadyHTTP: in.ReadyHTTP, ReadyTimeout: in.ReadyTimeout,
			Status: processStatus(in.Status)}
	}
	return out
}

func processStatus(st process.Status) protocol.ProcessStatus {
	return protocol.ProcessStatus{Name: st.Name, State: string(st.State), PID: st.PID, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt,
		ExitCode: st.ExitCode, Ready: st.Ready, Log: st.Log, Note: st.Note, Ports: st.Ports}
}

func (p processRoutes) Logs(name string, tail int) (protocol.ProcessLogs, error) {
	content, st, err := p.r.procs.Logs(name, tail)
	return protocol.ProcessLogs{Content: content, Status: processStatus(st)}, err
}

func (p processRoutes) Start(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return p.held(ctx, name, (*process.Manager).Start)
}

func (p processRoutes) Stop(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return p.held(ctx, name, (*process.Manager).Stop)
}

func (p processRoutes) Restart(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return p.held(ctx, name, (*process.Manager).Restart)
}

// held runs action as work that Close waits for. A Close whose ctx ends
// ends the wait for the ready gate.
func (p processRoutes) held(ctx context.Context, name string, action func(*process.Manager, context.Context, string) (process.Status, error)) (protocol.ProcessStatus, error) {
	if err := p.r.hold(); err != nil {
		return protocol.ProcessStatus{}, err
	}
	defer p.r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(p.r.base, cancel)()
	st, err := action(p.r.procs, ctx, name)
	return processStatus(st), err
}
