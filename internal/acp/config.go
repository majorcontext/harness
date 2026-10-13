package acp

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/majorcontext/harness/protocol"
)

const (
	optionModel  = "model"
	optionEffort = "effort"
	// effortDefault is the value of an effort that is not set.
	effortDefault = "default"
)

// efforts are the values of the effort option, in the order the client shows them.
var efforts = []string{effortDefault, "off", "minimal", "low", "medium", "high"}

type configValue struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}

type configOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Category     string        `json:"category"`
	Type         string        `json:"type"`
	CurrentValue string        `json:"currentValue"`
	Options      []configValue `json:"options"`
}

// configOptions returns the complete list of options for a session in state s.
// A model of s that models does not list is added to the values, so that the
// current value is always one of them.
func configOptions(models []protocol.Model, s protocol.Session) []configOption {
	values := make([]configValue, 0, len(models)+1)
	for _, m := range models {
		values = append(values, configValue{Value: m.ID, Name: m.ID})
	}
	if !slices.ContainsFunc(values, func(v configValue) bool { return v.Value == s.Model }) {
		values = append(values, configValue{Value: s.Model, Name: s.Model})
	}
	effortValues := make([]configValue, len(efforts))
	for i, e := range efforts {
		effortValues[i] = configValue{Value: e, Name: e}
	}
	return []configOption{
		{ID: optionModel, Name: "Model", Category: "model", Type: "select", CurrentValue: s.Model, Options: values},
		{ID: optionEffort, Name: "Effort", Category: "thought_level", Type: "select", CurrentValue: effortValue(s.Effort), Options: effortValues},
	}
}

func effortValue(effort string) string {
	if effort == "" {
		return effortDefault
	}
	return effort
}

// settingsPatch maps a config option change to a settings patch. It fails with
// invalid params for an option or a value that the session does not list.
func settingsPatch(models []protocol.Model, current protocol.Session, id string, value json.RawMessage) (protocol.SettingsPatch, error) {
	var v string
	if json.Unmarshal(value, &v) != nil {
		return protocol.SettingsPatch{}, invalidParams("value must be a string")
	}
	switch id {
	case optionModel:
		if v != current.Model && !slices.ContainsFunc(models, func(m protocol.Model) bool { return m.ID == v }) {
			return protocol.SettingsPatch{}, invalidParams("model is not served: " + v)
		}
		return protocol.SettingsPatch{Model: &v}, nil
	case optionEffort:
		if !slices.Contains(efforts, v) {
			return protocol.SettingsPatch{}, invalidParams("effort is not listed: " + v)
		}
		if v == effortDefault {
			v = ""
		}
		return protocol.SettingsPatch{Effort: &v}, nil
	}
	return protocol.SettingsPatch{}, invalidParams("no config option " + id)
}

func (a *agent) setConfigOption(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string          `json:"sessionId"`
		ConfigID  string          `json:"configId"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" || p.ConfigID == "" || len(p.Value) == 0 {
		return nil, invalidParams("sessionId, configId, and value are required")
	}
	h, err := a.held(p.SessionID)
	if err != nil {
		return nil, err
	}
	patch, err := settingsPatch(h.models, h.sess.Snapshot(), p.ConfigID, p.Value)
	if err != nil {
		return nil, err
	}
	s, err := h.sess.Update(ctx, patch)
	if err != nil {
		return nil, rpcError(err)
	}
	h.flush(ctx, s.HeadSeq)
	return map[string]any{"configOptions": configOptions(h.models, s)}, nil
}

type availableCommand struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Input       *commandHint `json:"input,omitempty"`
}

type commandHint struct {
	Hint string `json:"hint"`
}

// availableCommands lists the commands that a typed line can run.
func availableCommands(c protocol.Commands) []availableCommand {
	out := []availableCommand{}
	for _, e := range c.Commands {
		if !c.ServeSupport[e.Name].Supported {
			continue
		}
		cmd := availableCommand{Name: e.Name, Description: e.Summary}
		if cmd.Description == "" {
			cmd.Description = e.Name
		}
		if e.ArgHint != "" {
			cmd.Input = &commandHint{Hint: e.ArgHint}
		}
		out = append(out, cmd)
	}
	return out
}

// sessionResult is the answer of session/new. After it is queued, Serve calls
// AfterSent, which sends the commands of the session behind the answer.
type sessionResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
	after         func()
}

// AfterSent sends the updates that follow the answer.
func (r sessionResult) AfterSent() { r.after() }
