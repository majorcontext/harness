package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/process"
)

// handleProcessList answers GET /process: every declared process (config
// and runtime origin alike) with its live status. A server with no
// Options.Processes configured (Processes == nil) answers an empty list
// rather than 404 — the endpoint itself always exists, matching every
// other harness serve endpoint's stable-shape convention.
func (s *Server) handleProcessList(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Processes == nil {
		writeJSON(w, http.StatusOK, []process.Info{})
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Processes.List())
}

// processAction is the shape of Start/Stop/Restart, dispatched by
// handleProcessAction below.
type processAction func(ctx context.Context, name string) (process.Status, error)

func (s *Server) handleProcessStart(w http.ResponseWriter, r *http.Request) {
	s.handleProcessAction(w, r, func(ctx context.Context, name string) (process.Status, error) {
		return s.opts.Processes.Start(ctx, name)
	})
}

func (s *Server) handleProcessStop(w http.ResponseWriter, r *http.Request) {
	s.handleProcessAction(w, r, func(ctx context.Context, name string) (process.Status, error) {
		return s.opts.Processes.Stop(ctx, name)
	})
}

func (s *Server) handleProcessRestart(w http.ResponseWriter, r *http.Request) {
	s.handleProcessAction(w, r, func(ctx context.Context, name string) (process.Status, error) {
		return s.opts.Processes.Restart(ctx, name)
	})
}

// processLogsJSON is GET /process/{name}/logs' response shape — a
// console's processes panel wants both the trailing log content and the
// process's own current status in one round trip, rather than a second
// request to GET /process for the status half.
type processLogsJSON struct {
	Content string         `json:"content"`
	Status  process.Status `json:"status"`
}

// handleProcessLogs answers GET /process/{name}/logs?tail=N: the last N
// lines of name's log file (process.Manager.Logs' own default, 50, when
// tail is absent or not a positive integer) plus its current status. Same
// small-handler pattern as handleProcessAction, but Logs' own three-value
// return (content, status, error) doesn't fit that helper's
// Status-only processAction shape, so this gets its own body.
func (s *Server) handleProcessLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "process name is required")
		return
	}
	if s.opts.Processes == nil {
		writeErr(w, http.StatusNotFound, "no such process")
		return
	}
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	content, st, err := s.opts.Processes.Logs(name, tail)
	if err != nil {
		if errors.Is(err, process.ErrUnknownProcess) {
			writeErr(w, http.StatusNotFound, "no such process")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, processLogsJSON{Content: content, Status: st})
}

// handleProcessAction resolves {name} and, if Processes is configured and
// the name is a declared process, runs fn (Start/Stop/Restart) and
// answers its resulting Status. A nil Options.Processes, or a name naming
// no declared process (process.ErrUnknownProcess), both answer 404 — from
// a caller's perspective "no such process" and "processes not configured
// at all" are the same observable fact.
func (s *Server) handleProcessAction(w http.ResponseWriter, r *http.Request, fn processAction) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "process name is required")
		return
	}
	if s.opts.Processes == nil {
		writeErr(w, http.StatusNotFound, "no such process")
		return
	}
	st, err := fn(r.Context(), name)
	if err != nil {
		if errors.Is(err, process.ErrUnknownProcess) {
			writeErr(w, http.StatusNotFound, "no such process")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}
