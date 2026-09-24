package message

import (
	"encoding/json"
	"time"
)

// CommandStatus is the outcome of a resolved slash command.
type CommandStatus string

const (
	CommandAccepted    CommandStatus = "accepted"
	CommandSucceeded   CommandStatus = "succeeded"
	CommandFailed      CommandStatus = "failed"
	CommandRefused     CommandStatus = "refused"
	CommandUnsupported CommandStatus = "unsupported"
	CommandInterrupted CommandStatus = "interrupted"
)

// Terminal reports whether s ends a command. Only CommandAccepted does not.
func (s CommandStatus) Terminal() bool { return s != "" && s != CommandAccepted }

// CommandRecord is a slash command a person typed and its outcome. It is
// deliberately not a Message and not a Part: no transcoder can receive it.
type CommandRecord struct {
	ID              string          `json:"id"`
	Line            string          `json:"line"`
	Name            string          `json:"name"`
	Args            map[string]any  `json:"args,omitempty"`
	Source          PromptSource    `json:"source"`
	SourceID        string          `json:"source_id,omitempty"`
	SourceLabel     string          `json:"source_label,omitempty"`
	Status          CommandStatus   `json:"status"`
	Text            string          `json:"text,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	ResultTruncated bool            `json:"result_truncated,omitempty"`
	AfterMessageID  string          `json:"after_message_id,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}
