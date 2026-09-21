// Package command holds the slash-command vocabulary: what a human can
// type, and what each name resolves to. It performs no I/O and imports
// neither engine nor server, so a CLI, a server, and an HTTP client share
// one parse. See docs/design/slash-commands.md.
package command

// Kind separates the three things a name can resolve to.
type Kind string

const (
	// KindControl runs one session operation that already exists.
	KindControl Kind = "control"
	// KindFrontend changes which session the user talks to. It names no
	// Op and no route: the frontend that owns the pointer performs it.
	KindFrontend Kind = "frontend"
	// KindPrompt appends one user message. Reserved for the prompt-command
	// stage; no v1 entry uses it.
	KindPrompt Kind = "prompt"
)

// Op names a session operation abstractly. A route is one dispatcher's
// answer to an Op, not the operation itself.
type Op string

const (
	OpCompact        Op = "compact"
	OpSetModel       Op = "set_model"
	OpSetThinking    Op = "set_thinking"
	OpSetServiceTier Op = "set_service_tier"
	OpAbort          Op = "abort"
	OpSetGoal        Op = "set_goal"
	OpClearGoal      Op = "clear_goal"
	OpQueueList      Op = "queue_list"
	OpQueueClear     Op = "queue_clear"
	OpStatus         Op = "status"
	OpProcessList    Op = "process_list"
)

// ArgType is the type a dispatcher receives in Resolution.Args.
type ArgType string

const (
	ArgString ArgType = "string"
	ArgInt    ArgType = "int"
	// ArgRest takes the remainder of the line as one string. Only the
	// final ArgSpec of a Spec may use it.
	ArgRest ArgType = "rest"
)

// ArgSpec names one positional argument. Name is the key a dispatcher
// reads out of Resolution.Args.
type ArgSpec struct {
	Name     string
	Type     ArgType
	Optional bool
}

// Category groups commands in a menu.
type Category string

const (
	CategorySession  Category = "session"
	CategoryModel    Category = "model"
	CategoryInfo     Category = "info"
	CategoryFrontend Category = "frontend"
)

// Spec describes one command.
type Spec struct {
	Name        string
	Aliases     []string
	Kind        Kind
	Op          Op
	Summary     string
	ArgHint     string
	Args        []ArgSpec
	Category    Category
	Destructive bool
	Source      string
}
