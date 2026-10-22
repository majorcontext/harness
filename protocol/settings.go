package protocol

import "encoding/json"

// UnmarshalJSON preserves an explicit null allowlist as a change to all tools.
func (s *SettingsChanged) UnmarshalJSON(data []byte) error {
	type settings SettingsChanged
	var wire struct {
		settings
		AllowedTools json.RawMessage `json:"allowed_tools"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	next := SettingsChanged(wire.settings)
	if wire.AllowedTools != nil {
		var names []string
		if err := json.Unmarshal(wire.AllowedTools, &names); err != nil {
			return err
		}
		next.AllowedTools = &names
	}
	*s = next
	return nil
}
