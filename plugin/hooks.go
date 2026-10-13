package plugin

// ChatParams are the model request parameters that a hook can change.
// A nil field keeps the runtime default.
type ChatParams struct {
	Model       string   `json:"model,omitzero"` // provider/model reference
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
}

// ChatParamsRequest is the request of the chat.params hook.
type ChatParamsRequest struct {
	SessionID string     `json:"session_id"`
	Params    ChatParams `json:"params"`
}

// ChatParamsResponse is the response of the chat.params hook.
type ChatParamsResponse struct {
	Params ChatParams `json:"params"`
}

// ChatMessageRequest is the request of the chat.message hook.
type ChatMessageRequest struct {
	SessionID string  `json:"session_id"`
	Message   Message `json:"message"`
}

// ChatMessageResponse is the response of the chat.message hook.
type ChatMessageResponse struct {
	Message Message `json:"message"`
}

// SystemTransformRequest is the request of the system.transform hook.
type SystemTransformRequest struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitzero"` // provider/model reference
}

// SystemTransformResponse is the response of the system.transform hook; Segments are appended to the system prompt.
type SystemTransformResponse struct {
	Segments []string `json:"segments,omitempty"`
}

// ShellEnvRequest is the request of the shell.env hook.
type ShellEnvRequest struct {
	SessionID string `json:"session_id"`
	Tool      string `json:"tool"`
	Command   string `json:"command"`
	Dir       string `json:"dir,omitempty"` // working directory
}

// ShellEnvResponse is the response of the shell.env hook; Env is merged into the command environment.
type ShellEnvResponse struct {
	Env map[string]string `json:"env,omitempty"`
}
