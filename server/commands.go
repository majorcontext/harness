package server

import (
	"net/http"

	"github.com/majorcontext/harness/command"
)

// route is the serve-mode answer to one Op. A route is this dispatcher's
// mapping, not the operation: cmd/harness maps the same Op to a method
// call. See docs/design/slash-commands.md §5.
type route struct {
	method string
	path   string
}

var opRoutes = map[command.Op]route{
	command.OpCompact:        {"POST", "/session/{id}/compact"},
	command.OpSetModel:       {"POST", "/session/{id}/model"},
	command.OpSetThinking:    {"POST", "/session/{id}/thinking"},
	command.OpSetServiceTier: {"POST", "/session/{id}/service-tier"},
	command.OpAbort:          {"POST", "/session/{id}/abort"},
	command.OpSetGoal:        {"POST", "/session/{id}/goal"},
	command.OpClearGoal:      {"DELETE", "/session/{id}/goal"},
	command.OpQueueList:      {"GET", "/session/{id}/queue"},
	command.OpQueueClear:     {"DELETE", "/session/{id}/queue"},
	command.OpStatus:         {"GET", "/session/{id}"},
	command.OpProcessList:    {"GET", "/process"},
}

// serveModeOps declares which control Ops serve mode resolves through
// its own routes, total over every command.Op: a new Op with no entry
// here fails TestServeModeOpsTotal instead of silently reaching a
// client as supported. queue-clear stays false because it is out of
// scope for this plan; DELETE /session/{id}/queue already exists and a
// future task can flip it. See docs/design/slash-commands.md §5.
var serveModeOps = map[command.Op]bool{
	command.OpCompact:        true,
	command.OpSetModel:       true,
	command.OpSetThinking:    true,
	command.OpSetServiceTier: true,
	command.OpAbort:          true,
	command.OpSetGoal:        true,
	command.OpClearGoal:      true,
	command.OpQueueList:      true,
	command.OpQueueClear:     false,
	command.OpStatus:         true,
	command.OpProcessList:    true,
}

// serveUnsupportedReason is published verbatim to the person typing. It
// names no route and no internal term.
const serveUnsupportedReason = "Not available in this client."

// serveSupport reports whether serve mode resolves spec. A frontend
// command names no route: the frontend owns the session pointer, not
// serve mode.
func serveSupport(spec *command.Spec) (supported bool, reason string) {
	if spec.Kind == command.KindFrontend {
		return false, serveUnsupportedReason
	}
	if !serveModeOps[spec.Op] {
		return false, serveUnsupportedReason
	}
	return true, ""
}

type commandArgJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

type commandEntryJSON struct {
	Name     string           `json:"name"`
	Aliases  []string         `json:"aliases,omitempty"`
	Kind     string           `json:"kind"`
	Op       string           `json:"op,omitempty"`
	Summary  string           `json:"summary"`
	ArgHint  string           `json:"arg_hint,omitempty"`
	Args     []commandArgJSON `json:"args,omitempty"`
	Category string           `json:"category"`
	// AvailableDuringTask is a pointer so a frontend entry omits it
	// entirely, like op/method/path: the field describes route behavior
	// and a frontend command has no route. A client reading an older
	// server that omits it for a CONTROL command must assume false.
	AvailableDuringTask *bool  `json:"available_during_task,omitempty"`
	Destructive         bool   `json:"destructive,omitempty"`
	Method              string `json:"method,omitempty"`
	Path                string `json:"path,omitempty"`
}

type serveSupportJSON struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// handleCommands returns the resolved registry. A frontend renders its
// menu and its argument hints from this response alone, but a client
// that resolves commands itself, such as serve mode's console, also
// needs serve_support to know which entries it can run without a
// frontend in front of it.
func (s *Server) handleCommands(w http.ResponseWriter, _ *http.Request) {
	specs := command.NewRegistry().All()
	out := make([]commandEntryJSON, 0, len(specs))
	serveSupportOut := make(map[string]serveSupportJSON, len(specs))
	for _, spec := range specs {
		supported, reason := serveSupport(spec)
		serveSupportOut[spec.Name] = serveSupportJSON{Supported: supported, Reason: reason}
		e := commandEntryJSON{
			Name:        spec.Name,
			Aliases:     spec.Aliases,
			Kind:        string(spec.Kind),
			Op:          string(spec.Op),
			Summary:     spec.Summary,
			ArgHint:     spec.ArgHint,
			Category:    string(spec.Category),
			Destructive: spec.Destructive,
		}
		for _, a := range spec.Args {
			e.Args = append(e.Args, commandArgJSON{Name: a.Name, Type: string(a.Type), Optional: a.Optional})
		}
		if rt, ok := opRoutes[spec.Op]; ok {
			e.Method, e.Path = rt.method, rt.path
			avail := spec.AvailableDuringTask
			e.AvailableDuringTask = &avail
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, struct {
		Commands     []commandEntryJSON          `json:"commands"`
		ServeSupport map[string]serveSupportJSON `json:"serve_support"`
	}{Commands: out, ServeSupport: serveSupportOut})
}
