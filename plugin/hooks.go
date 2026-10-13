package plugin

// ChatParams are the parameters of one model call. A nil field keeps the
// value of the call, or the runtime default.
type ChatParams struct {
	Model       string   `json:"model,omitzero"` // provider/model reference, not an alias; empty keeps the model of the call
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
}

// ChatParamsRequest is the request of the chat.params hook.
type ChatParamsRequest struct {
	SessionID string     `json:"session_id"`
	Params    ChatParams `json:"params"`
}

// ChatParamsResponse is the response of the chat.params hook. Params replaces
// all the parameters: start from a copy of the request Params.
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

// SystemTransformResponse is the response of the system.transform hook.
type SystemTransformResponse struct {
	Segments []string `json:"segments,omitempty"` // added to the end of the system prompt
}

// ShellEnvRequest is the request of the shell.env hook.
type ShellEnvRequest struct {
	SessionID string `json:"session_id"`
	Tool      string `json:"tool"` // bash
	Command   string `json:"command"`
	Dir       string `json:"dir,omitempty"` // working directory
}

// ShellEnvResponse is the response of the shell.env hook.
type ShellEnvResponse struct {
	Env map[string]string `json:"env,omitempty"` // added to the command environment; a later plugin wins
}
