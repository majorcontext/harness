package protocol

import "time"

// ProcessStatus is a point-in-time snapshot of one managed process. State is
// starting, ready, running, exited, or stopped, and empty for a process that
// was declared and never started.
type ProcessStatus struct {
	Name       string    `json:"name"`
	State      string    `json:"state,omitempty"`
	PID        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// ExitCode is set once the process has been reaped.
	ExitCode int    `json:"exit_code,omitzero"`
	Ready    bool   `json:"ready"`
	Log      string `json:"log"`
	// Note is a human-readable annotation, such as a ready-gate timeout.
	Note  string `json:"note,omitempty"`
	Ports []int  `json:"ports,omitempty"`
}

// ProcessInfo is the definition of a declared process and its status. It
// names the variables of its environment and never holds their values.
type ProcessInfo struct {
	Name         string        `json:"name"`
	Origin       string        `json:"origin"`
	Command      []string      `json:"command"`
	Dir          string        `json:"dir,omitempty"`
	EnvNames     []string      `json:"env_names,omitempty"`
	Ports        []int         `json:"ports,omitempty"`
	ReadyRegex   string        `json:"ready_regex,omitempty"`
	ReadyPort    int           `json:"ready_port,omitempty"`
	ReadyHTTP    string        `json:"ready_http,omitempty"`
	ReadyTimeout string        `json:"ready_timeout"`
	Status       ProcessStatus `json:"status"`
}

// ProcessLogs is the answer of GET /processes/{name}/logs: the last lines of
// the log and the status of the process.
type ProcessLogs struct {
	Content string        `json:"content"`
	Status  ProcessStatus `json:"status"`
}
