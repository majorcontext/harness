package server

import (
	"net/http"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/protocol"
)

// Route is one route of the HTTP API. Request and Response hold a zero value
// of the JSON body type, or nil when the route has none. Query names the
// optional query parameters. Status is the success status.
type Route struct {
	// Name identifies the route, and is its OpenAPI operation ID.
	Name     string
	Method   string
	Path     string
	Query    []Param
	Request  any
	Response any
	Status   int
	// Stream marks the SSE form of a route: Response is the type of one data frame.
	Stream bool
	// WorkDir marks a route that the handler serves only with a WorkDir.
	WorkDir bool
	// Ops lists the control commands that this route runs.
	Ops []command.Op
}

// Param is an optional query parameter. Integer is true for a number.
type Param struct {
	Name    string
	Integer bool
}

var (
	afterParam = Param{Name: "after"}
	limitParam = Param{Name: "limit", Integer: true}
)

// Table lists every route of the handler. The handler registers its mux from
// this table, and the generator writes the OpenAPI document from it.
var Table = []Route{
	{Name: "createSession", Method: "POST", Path: "/sessions", Request: protocol.CreateSession{}, Response: protocol.Session{}, Status: 201},
	{Name: "listSessions", Method: "GET", Path: "/sessions", Query: []Param{afterParam, limitParam}, Response: protocol.SessionPage{}, Status: 200},
	{Name: "getSession", Method: "GET", Path: "/sessions/{id}", Response: protocol.Session{}, Status: 200, Ops: []command.Op{command.OpStatus}},
	{Name: "endSession", Method: "DELETE", Path: "/sessions/{id}", Status: 204},
	{Name: "updateSession", Method: "PATCH", Path: "/sessions/{id}", Request: protocol.SettingsPatch{}, Response: protocol.Session{}, Status: 200,
		Ops: []command.Op{command.OpSetModel, command.OpSetThinking, command.OpSetServiceTier}},
	{Name: "submitInput", Method: "POST", Path: "/sessions/{id}/inputs", Request: protocol.Input{}, Response: protocol.Admitted{}, Status: 201},
	{Name: "listInputs", Method: "GET", Path: "/sessions/{id}/inputs", Response: []string{}, Status: 200, Ops: []command.Op{command.OpQueueList}},
	{Name: "withdrawInput", Method: "DELETE", Path: "/sessions/{id}/inputs/{input}", Status: 204},
	{Name: "interruptSession", Method: "POST", Path: "/sessions/{id}/interrupt", Request: protocol.Interrupt{}, Status: 204, Ops: []command.Op{command.OpAbort}},
	{Name: "compactSession", Method: "POST", Path: "/sessions/{id}/compact", Request: protocol.Compact{}, Response: protocol.Compacted{}, Status: 200, Ops: []command.Op{command.OpCompact}},
	{Name: "resolveRequest", Method: "POST", Path: "/sessions/{id}/requests/{request}", Request: protocol.Resolution{}, Status: 204},
	{Name: "setGoal", Method: "PUT", Path: "/sessions/{id}/goal", Request: protocol.Goal{}, Response: protocol.Session{}, Status: 200, Ops: []command.Op{command.OpSetGoal}},
	{Name: "clearGoal", Method: "DELETE", Path: "/sessions/{id}/goal", Status: 204, Ops: []command.Op{command.OpClearGoal}},
	{Name: "listEvents", Method: "GET", Path: "/sessions/{id}/events", Query: []Param{afterParam, limitParam}, Response: protocol.EventPage{}, Status: 200},
	{Name: "streamEvents", Method: "GET", Path: "/sessions/{id}/events", Query: []Param{afterParam}, Response: protocol.Event{}, Status: 200, Stream: true},
	{Name: "listMessages", Method: "GET", Path: "/sessions/{id}/messages", Query: []Param{{Name: "before", Integer: true}, limitParam}, Response: protocol.MessagePage{}, Status: 200},
	{Name: "listModels", Method: "GET", Path: "/models", Response: []protocol.Model{}, Status: 200},
	{Name: "listCommands", Method: "GET", Path: "/commands", Response: protocol.Commands{}, Status: 200},
	{Name: "listProcesses", Method: "GET", Path: "/processes", Response: []protocol.ProcessInfo{}, Status: 200, Ops: []command.Op{command.OpProcessList}},
	{Name: "startProcess", Method: "POST", Path: "/processes/{name}/start", Response: protocol.ProcessStatus{}, Status: 200},
	{Name: "stopProcess", Method: "POST", Path: "/processes/{name}/stop", Response: protocol.ProcessStatus{}, Status: 200},
	{Name: "restartProcess", Method: "POST", Path: "/processes/{name}/restart", Response: protocol.ProcessStatus{}, Status: 200},
	{Name: "processLogs", Method: "GET", Path: "/processes/{name}/logs", Query: []Param{{Name: "tail", Integer: true}}, Response: protocol.ProcessLogs{}, Status: 200},
	{Name: "workspaceChanges", Method: "GET", Path: "/workspace/changes", Query: []Param{{Name: "scope"}, {Name: "dir"}}, Response: protocol.WorkspaceChanges{}, Status: 200, WorkDir: true},
	{Name: "health", Method: "GET", Path: "/health", Response: protocol.Health{}, Status: 200},
}

// bind registers each route of Table on mux with its handler in handlers,
// and records the route of each control command. A route with no handler,
// or a handler that no route names, is a programming error.
func (h *handler[S]) bind(mux *http.ServeMux, handlers map[string]http.HandlerFunc) {
	registered := map[string]bool{}
	for _, r := range Table {
		f, ok := handlers[r.Name]
		if !ok {
			panic("server: route " + r.Name + " has no handler")
		}
		delete(handlers, r.Name)
		if r.WorkDir && h.workDir == "" {
			continue
		}
		for _, op := range r.Ops {
			h.routes[op] = route{r.Method, r.Path}
		}
		if pattern := r.Method + " " + r.Path; !registered[pattern] {
			registered[pattern] = true
			mux.HandleFunc(pattern, f)
		}
	}
	for name := range handlers {
		panic("server: handler " + name + " has no route")
	}
}
