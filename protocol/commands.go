package protocol

// SourceTyped marks a typed input, the only kind that can be a slash command.
const SourceTyped = "typed"

// Slash command statuses.
const (
	CommandAccepted    = "accepted"
	CommandSucceeded   = "succeeded"
	CommandFailed      = "failed"
	CommandRefused     = "refused"
	CommandUnsupported = "unsupported"
	CommandInterrupted = "interrupted"
)

// Commands is the slash-command menu, sorted by name. ServeSupport says
// whether a typed line runs each command.
type Commands struct {
	Commands        []CommandEntry            `json:"commands"`
	ServeSupport    map[string]CommandSupport `json:"serve_support"`
	DiscoveryErrors []string                  `json:"discovery_errors,omitempty"`
}

// CommandEntry is one slash command: control, frontend, or prompt. A
// control command names its operation and the route of that operation.
type CommandEntry struct {
	Name                string       `json:"name"`
	Aliases             []string     `json:"aliases,omitempty"`
	Kind                string       `json:"kind"`
	Op                  string       `json:"op,omitempty"`
	Summary             string       `json:"summary"`
	ArgHint             string       `json:"arg_hint,omitempty"`
	Args                []CommandArg `json:"args,omitempty"`
	Category            string       `json:"category"`
	AvailableDuringTask *bool        `json:"available_during_task,omitempty"`
	Destructive         bool         `json:"destructive,omitempty"`
	Method              string       `json:"method,omitempty"`
	Path                string       `json:"path,omitempty"`
}

// CommandArg is one positional argument: string, int, or rest of the line.
type CommandArg struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

// CommandSupport says whether a typed command runs, or why not.
type CommandSupport struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}
