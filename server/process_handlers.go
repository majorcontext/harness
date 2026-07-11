package server

import (
	"context"
	"errors"
	"net/http"

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
