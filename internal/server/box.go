package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/internal/process"
	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/protocol"
)

// boxHandlers returns the handlers of the routes of the box that hosts the
// runtime: its processes and its work tree.
func (h *handler[S]) boxHandlers() map[string]http.HandlerFunc {
	m := map[string]http.HandlerFunc{
		"listProcesses": h.serve(func(w http.ResponseWriter, _ *http.Request) error {
			list := []protocol.ProcessInfo{}
			if h.procs != nil {
				list = h.procs.List()
			}
			reply(w, http.StatusOK, list)
			return nil
		}),
		"processLogs": h.process(logs),
		"workspaceChanges": h.serve(func(w http.ResponseWriter, r *http.Request) error {
			q := r.URL.Query()
			c, err := workspace.Changes(r.Context(), h.workDir, q.Get("dir"), q.Get("scope"))
			if err != nil {
				return err
			}
			replyRaw(w, c)
			return nil
		}),
	}
	for name, run := range map[string]func(Processes, context.Context, string) (protocol.ProcessStatus, error){
		"startProcess": Processes.Start, "stopProcess": Processes.Stop, "restartProcess": Processes.Restart,
	} {
		m[name] = h.process(func(p Processes, w http.ResponseWriter, r *http.Request) error {
			st, err := run(p, r.Context(), r.PathValue("name"))
			if err != nil {
				return err
			}
			reply(w, http.StatusOK, st)
			return nil
		})
	}
	return m
}

// process runs f over the processes. Without them, every name is unknown.
func (h *handler[S]) process(f func(Processes, http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return h.serve(func(w http.ResponseWriter, r *http.Request) error {
		if h.procs == nil {
			return fmt.Errorf("%w %q", process.ErrUnknownProcess, r.PathValue("name"))
		}
		return f(h.procs, w, r)
	})
}

// logs answers the last tail lines of the log, 50 when tail is not a
// positive number, and the status of the process.
func logs(m Processes, w http.ResponseWriter, r *http.Request) error {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	out, err := m.Logs(r.PathValue("name"), tail)
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, out)
	return nil
}
