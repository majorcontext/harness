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
	command.OpMCP:            {"POST", "/session/{id}/mcp"},
}

type commandArgJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

type commandEntryJSON struct {
	Name        string           `json:"name"`
	Aliases     []string         `json:"aliases,omitempty"`
	Kind        string           `json:"kind"`
	Op          string           `json:"op,omitempty"`
	Summary     string           `json:"summary"`
	ArgHint     string           `json:"arg_hint,omitempty"`
	Args        []commandArgJSON `json:"args,omitempty"`
	Category    string           `json:"category"`
	Destructive bool             `json:"destructive,omitempty"`
	Method      string           `json:"method,omitempty"`
	Path        string           `json:"path,omitempty"`
}

// handleCommands returns the resolved registry. A frontend renders its
// menu and its argument hints from this response alone.
func (s *Server) handleCommands(w http.ResponseWriter, _ *http.Request) {
	specs := command.NewRegistry().All()
	out := make([]commandEntryJSON, 0, len(specs))
	for _, spec := range specs {
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
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, struct {
		Commands []commandEntryJSON `json:"commands"`
	}{Commands: out})
}
