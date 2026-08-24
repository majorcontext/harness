package message

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ModelRef identifies a model as "provider/model", e.g.
// "anthropic/claude-fable-5" or "amazon-bedrock/us.anthropic.claude-opus-4-8".
// The model portion may itself contain slashes; the split is on the first one.
//
// User-defined aliases ("fast", "smart") are resolved to ModelRefs by config
// before they reach this type.
type ModelRef struct {
	Provider string
	Model    string
}

// ParseModelRef parses "provider/model". Both segments must be non-empty.
func ParseModelRef(s string) (ModelRef, error) {
	provider, model, ok := strings.Cut(s, "/")
	if !ok || provider == "" || model == "" {
		return ModelRef{}, fmt.Errorf(`message: model ref %q is not of the form "provider/model"`, s)
	}
	return ModelRef{Provider: provider, Model: model}, nil
}

func (r ModelRef) String() string {
	if r.IsZero() {
		return ""
	}
	return r.Provider + "/" + r.Model
}

func (r ModelRef) IsZero() bool { return r == ModelRef{} }

// MarshalJSON encodes the ref as its compact string form.
func (r ModelRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

func (r *ModelRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*r = ModelRef{}
		return nil
	}
	ref, err := ParseModelRef(s)
	if err != nil {
		return err
	}
	*r = ref
	return nil
}
