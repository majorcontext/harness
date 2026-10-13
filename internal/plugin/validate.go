package plugin

import (
	"encoding/json"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	pub "github.com/majorcontext/harness/plugin"
)

// The response types below decode like their public types and also reject
// what the canonical types reject: a bad model reference or a malformed part
// fails the call.

// chatParamsResponse and chatMessageResponse record whether the response
// carries its key: a response without it changes nothing.
type chatParamsResponse struct {
	pub.ChatParamsResponse
	has bool
}

func (r *chatParamsResponse) UnmarshalJSON(b []byte) error {
	var v struct {
		Params *struct {
			Model modelref.ModelRef `json:"model"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	r.has = v.Params != nil
	return json.Unmarshal(b, &r.ChatParamsResponse)
}

type chatMessageResponse struct {
	pub.ChatMessageResponse
	has bool
}

func (r *chatMessageResponse) UnmarshalJSON(b []byte) error {
	var v struct {
		Message *struct{} `json:"message"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	r.has = v.Message != nil
	return json.Unmarshal(b, &r.ChatMessageResponse)
}

type toolAfterResponse struct{ pub.ToolExecuteAfterResponse }

func (r *toolAfterResponse) UnmarshalJSON(b []byte) error {
	if err := checkParts(b); err != nil {
		return err
	}
	return json.Unmarshal(b, &r.ToolExecuteAfterResponse)
}

type toolExecuteResponse struct{ pub.ToolExecuteResponse }

func (r *toolExecuteResponse) UnmarshalJSON(b []byte) error {
	if err := checkParts(b); err != nil {
		return err
	}
	return json.Unmarshal(b, &r.ToolExecuteResponse)
}

func checkParts(b []byte) error {
	var v struct {
		Output message.Parts `json:"output"`
	}
	return json.Unmarshal(b, &v)
}
