package message

// ToolChange names the tools that leave and join the callable set, in a request-only part.
type ToolChange struct {
	Removed []string `json:"removed,omitempty"`
	Added   []string `json:"added,omitempty"`
}

func (*ToolChange) partType() PartType { return PartToolChange }

// PartToolChange is the JSON discriminator for a ToolChange part.
const PartToolChange PartType = "tool_change"
