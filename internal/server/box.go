package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/internal/workspace"
	"github.com/majorcontext/harness/process"
)

// box serves the routes of the box that hosts the runtime: its processes
// and its work tree.
func (h *handler[S]) box(mux *http.ServeMux) {
	mux.HandleFunc("GET /processes", h.serve(func(w http.ResponseWriter, _ *http.Request) error {
		list := []process.Info{}
		if m := h.rt.Processes(); m != nil {
			list = m.List()
		}
		reply(w, http.StatusOK, list)
		return nil
	}))
	for action, run := range map[string]func(*process.Manager, context.Context, string) (process.Status, error){
		"start": (*process.Manager).Start, "stop": (*process.Manager).Stop, "restart": (*process.Manager).Restart,
	} {
		mux.HandleFunc("POST /processes/{name}/"+action, h.process(func(m *process.Manager, w http.ResponseWriter, r *http.Request) error {
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

// process runs f over the process manager. Without one, every name is unknown.
func (h *handler[S]) process(f func(*process.Manager, http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return h.serve(func(w http.ResponseWriter, r *http.Request) error {
		m := h.rt.Processes()
		if m == nil {
			return fmt.Errorf("%w %q", process.ErrUnknownProcess, r.PathValue("name"))
		}
		return f(m, w, r)
	})
}

// logs answers the last tail lines of the log, 50 when tail is not a
// positive number, and the status of the process.
func logs(m *process.Manager, w http.ResponseWriter, r *http.Request) error {
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
