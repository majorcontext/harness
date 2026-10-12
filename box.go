package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/majorcontext/harness/internal/process"
	"github.com/majorcontext/harness/protocol"
)

// Health returns the body of GET /health.
func (r *Runtime) Health() protocol.Health { return r.health }

// Logger returns the logger that receives the log lines of the runtime.
func (r *Runtime) Logger() *slog.Logger { return r.log }

// WorkDir returns the directory of the coding agent, or "" without one.
func (r *Runtime) WorkDir() string { return r.workDir }

// Processes lists every process of the box with its definition and status. It
// lists none without a WorkDir.
func (r *Runtime) Processes() []protocol.ProcessInfo {
	if r.procs == nil {
		return []protocol.ProcessInfo{}
	}
	list := r.procs.List()
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

// ProcessLogs returns the last tail lines of the log of process name, 50 when
// tail is not positive, and its status. An unknown name fails with
// ErrProcessNotFound.
func (r *Runtime) ProcessLogs(name string, tail int) (protocol.ProcessLogs, error) {
	if r.procs == nil {
		return protocol.ProcessLogs{}, unknownProcess(name)
	}
	content, st, err := r.procs.Logs(name, tail)
	return protocol.ProcessLogs{Content: content, Status: processStatus(st)}, processError(err)
}

// StartProcess starts process name and returns its status once it is ready. It
// is work that Close waits for, and fails with ErrDraining after Close starts.
func (r *Runtime) StartProcess(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return r.heldProcess(ctx, name, (*process.Manager).Start)
}

// StopProcess stops process name and returns its status. It is work that Close
// waits for, and fails with ErrDraining after Close starts.
func (r *Runtime) StopProcess(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return r.heldProcess(ctx, name, (*process.Manager).Stop)
}

// RestartProcess restarts process name and returns its status once it is
// ready. It is work that Close waits for, and fails with ErrDraining after
// Close starts.
func (r *Runtime) RestartProcess(ctx context.Context, name string) (protocol.ProcessStatus, error) {
	return r.heldProcess(ctx, name, (*process.Manager).Restart)
}

func unknownProcess(name string) error {
	return processError(fmt.Errorf("%w %q", process.ErrUnknownProcess, name))
}

// processError returns the unknown name of the manager as an error that matches ErrProcessNotFound and keeps the text.
func processError(err error) error {
	if errors.Is(err, process.ErrUnknownProcess) {
		return &protocol.Error{Code: protocol.CodeProcessNotFound, Message: err.Error()}
	}
	return err
}

// heldProcess runs action as work that Close waits for. A Close whose ctx ends
// ends the wait for the ready gate.
func (r *Runtime) heldProcess(ctx context.Context, name string, action func(*process.Manager, context.Context, string) (process.Status, error)) (protocol.ProcessStatus, error) {
	if r.procs == nil {
		return protocol.ProcessStatus{}, unknownProcess(name)
	}
	if err := r.hold(); err != nil {
		return protocol.ProcessStatus{}, err
	}
	defer r.group.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.base, cancel)()
	st, err := action(r.procs, ctx, name)
	return processStatus(st), processError(err)
}
