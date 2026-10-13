package acp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

const (
	optionModel  = "model"
	optionEffort = "effort"
	// effortDefault is the value of an effort that is not set.
	effortDefault = "default"
)

// effortNames are the labels of the effort values.
var effortNames = map[string]string{effortDefault: "Default", "off": "Off", "minimal": "Minimal", "low": "Low", "medium": "Medium", "high": "High"}

// providerLabels are the group labels of providers whose key is not a plain
// capitalized word.
var providerLabels = map[string]string{"openai": "OpenAI"}

// otherGroup is the group of a current model whose provider lists no model.
const otherGroup = "Other"

// descriptionLimit is the length at which a model description is cut.
const descriptionLimit = 100

type configValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type configGroup struct {
	Group   string        `json:"group"`
	Name    string        `json:"name"`
	Options []configValue `json:"options"`
}

type configOption struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	Type         string `json:"type"`
	CurrentValue string `json:"currentValue"`
	// Options is a []configValue, or a []configGroup for a grouped option.
	Options any `json:"options"`
}

// providerLabel is the label of a provider group: the key with each word
// capitalized, unless providerLabels names it.
func providerLabel(key string) string {
	if l, ok := providerLabels[key]; ok {
		return l
	}
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// shortDescription is the first sentence of d, cut at descriptionLimit.
func shortDescription(d string) string {
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i]
	}
	d = strings.TrimSuffix(d, ".")
	if len(d) <= descriptionLimit {
		return d
	}
	cut := strings.LastIndexByte(d[:descriptionLimit], ' ')
	if cut <= 0 {
		cut = descriptionLimit
	}
	return d[:cut] + "…"
}

// modelValues returns the models as options grouped by provider, in the order
// of models. A model of s that models does not list joins the group of its
// provider, or the group Other.
func modelValues(models []protocol.Model, s protocol.Session) []configGroup {
	var groups []configGroup
	index := map[string]int{}
	add := func(key, label string, v configValue) {
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, configGroup{Group: key, Name: label})
		}
		groups[i].Options = append(groups[i].Options, v)
	}
	for _, m := range models {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		desc := m.ID
		if d := shortDescription(m.Description); d != "" {
			desc += " · " + d
		}
		add(m.Provider, providerLabel(m.Provider), configValue{Value: m.ID, Name: name, Description: desc})
	}
	if !slices.ContainsFunc(models, func(m protocol.Model) bool { return m.ID == s.Model }) {
		key := otherGroup
		if p, _, ok := strings.Cut(s.Model, "/"); ok {
			if _, listed := index[p]; listed {
				key = p
			}
		}
		label := otherGroup
		if key != otherGroup {
			label = providerLabel(key)
		}
		add(key, label, configValue{Value: s.Model, Name: s.Model})
	}
	return groups
}

// effortChoices returns the effort values that the model of s offers: the
// default, the levels of the model, and the current effort, so that the
// current value is always one of them.
func effortChoices(models []protocol.Model, s protocol.Session) []string {
	out := []string{effortDefault}
	for _, m := range models {
		if m.ID == s.Model {
			for _, e := range m.Efforts {
				if _, known := effortNames[e]; known && !slices.Contains(out, e) {
					out = append(out, e)
				}
			}
		}
	}
	if cur := effortValue(s.Effort); !slices.Contains(out, cur) {
		out = append(out, cur)
	}
	return out
}

// configOptions returns the complete list of options for a session in state s.
func configOptions(models []protocol.Model, s protocol.Session) []configOption {
	choices := effortChoices(models, s)
	effortValues := make([]configValue, len(choices))
	for i, e := range choices {
		name := effortNames[e]
		if name == "" {
			name = e
		}
		effortValues[i] = configValue{Value: e, Name: name}
	}
	return []configOption{
		{ID: optionModel, Name: "Model", Category: "model", Type: "select", CurrentValue: s.Model, Options: modelValues(models, s)},
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
		if !slices.Contains(effortChoices(models, current), v) {
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
