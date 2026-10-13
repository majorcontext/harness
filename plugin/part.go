package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The Part types that have typed fields. Every other type keeps its raw JSON.
const (
	PartText = "text" // plain text
	PartBlob = "blob" // binary content such as an image
)

// Part is one content block of a message or of a tool result.
//
// Text and blob parts use the typed fields. Any other type keeps its JSON in
// Raw and passes through unchanged.
type Part struct {
	Type      string          // PartText, PartBlob, or another wire type
	Text      string          // PartText only
	MediaType string          // PartBlob only
	Data      []byte          // inline bytes of a PartBlob part; not set with URL
	URL       string          // location of a PartBlob part; not set with Data
	Raw       json.RawMessage // JSON of a part of another type
}

// TextPart returns a text part.
func TextPart(text string) Part { return Part{Type: PartText, Text: text} }

// MarshalJSON encodes p. A part of another type than text or blob encodes as Raw.
func (p Part) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case PartText:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{PartText, p.Text})
	case PartBlob:
		return json.Marshal(struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      []byte `json:"data,omitempty"`
			URL       string `json:"url,omitempty"`
		}{PartBlob, p.MediaType, p.Data, p.URL})
	}
	if len(p.Raw) == 0 {
		return nil, fmt.Errorf("plugin: part of type %q has no raw JSON", p.Type)
	}
	return bytes.Clone(p.Raw), nil
}

// UnmarshalJSON decodes a part and keeps the raw JSON of a type without typed fields.
func (p *Part) UnmarshalJSON(b []byte) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	*p = Part{Type: head.Type}
	switch head.Type {
	case PartText:
		var v struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		p.Text = v.Text
	case PartBlob:
		var v struct {
			MediaType string `json:"media_type"`
			Data      []byte `json:"data"`
			URL       string `json:"url"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		p.MediaType, p.Data, p.URL = v.MediaType, v.Data, v.URL
	default:
		p.Raw = bytes.Clone(b)
	}
	return nil
}
