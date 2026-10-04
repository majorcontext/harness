package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/process"
)

// box serves the routes of the box that hosts the runtime: its processes
// and its work tree.
func (h *handler[S]) box(mux *http.ServeMux) {
	h.handle(mux, "GET /processes", h.serve(func(w http.ResponseWriter, _ *http.Request) error {
		list := []process.Info{}
		if h.procs != nil {
			list = h.procs.List()
		}
		reply(w, http.StatusOK, list)
		return nil
	}), command.OpProcessList)
	for action, run := range map[string]func(Processes, context.Context, string) (process.Status, error){
		"start": Processes.Start, "stop": Processes.Stop, "restart": Processes.Restart,
	} {
		mux.HandleFunc("POST /processes/{name}/"+action, h.process(func(m Processes, w http.ResponseWriter, r *http.Request) error {
			st, err := run(m, r.Context(), r.PathValue("name"))
			if err != nil {
				return err
			}
			reply(w, http.StatusOK, st)
			return nil
		}))
	}
	mux.HandleFunc("GET /processes/{name}/logs", h.process(logs))
	if h.workDir != "" {
		mux.HandleFunc("GET /workspace/changes", h.serve(func(w http.ResponseWriter, r *http.Request) error {
			q := r.URL.Query()
			c, err := workspace.Changes(r.Context(), h.workDir, q.Get("dir"), q.Get("scope"))
			if err != nil {
				return err
			}
			replyRaw(w, c)
			return nil
		}))
	}
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
	content, st, err := m.Logs(r.PathValue("name"), tail)
	if err != nil {
		return err
	}
	reply(w, http.StatusOK, struct {
		Content string         `json:"content"`
		Status  process.Status `json:"status"`
	}{content, st})
	return nil
}
