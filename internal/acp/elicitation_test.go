package acp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/majorcontext/harness/internal/acp/rpc"
	"github.com/majorcontext/harness/protocol"
)

func TestFormOfMapsMultiSelectAndFreeTextQuestions(t *testing.T) {
	payload := json.RawMessage(`{"questions":[
		{"question":"Which tools?","header":"Tools","multiSelect":true,"options":[{"label":"lint"},{"label":"test","description":"runs tests"}]},
		{"question":"Name it?"}]}`)
	f, ok := formOf(payload)
	if !ok {
		t.Fatal("formOf reported no form")
	}
	want := map[string]any{
		"type": "object", "required": []string{"question_2"},
		"properties": map[string]any{
			"question_1": map[string]any{"type": "array", "title": "Tools", "description": "Which tools?", "minItems": 1,
				"items": map[string]any{"anyOf": []any{
					map[string]any{"const": "lint", "title": "lint"},
					map[string]any{"const": "test", "title": "test", "description": "runs tests"}}}},
			"question_1_other": map[string]any{"type": "string", "title": "Other", "description": "Your own answer to: Which tools?"},
			"question_2":       map[string]any{"type": "string", "title": "Name it?", "description": "Name it?"},
		},
	}
	if !reflect.DeepEqual(f.schema, want) || f.message != "Which tools?\nName it?" {
		t.Errorf("form = %v / %q, want %v", f.schema, f.message, want)
	}
	answer, ok := f.answer(map[string]any{"question_1": []any{"lint", "test"}, "question_2": "x"})
	if string(answer) != `{"Name it?":"x","Which tools?":"lint, test"}` || !ok {
		t.Errorf("answer = %s, %v", answer, ok)
	}
	if _, ok := f.answer(map[string]any{}); ok {
		t.Error("an accept with no value gave an answer")
	}
}

func TestResolvedNamesWhoDismissedTheQuestion(t *testing.T) {
	var got toolUpdateBody
	h := &held{id: "ses_1", elicits: map[string]*rpc.Pending{}, send: func(_ string, params any) error {
		got = params.(map[string]any)["update"].(toolUpdateBody)
		return nil
	}}
	data, _ := json.Marshal(protocol.RequestResolved{RequestID: "call_q", Resolution: protocol.ResolutionDismissed, Source: protocol.SourceBox, SourceLabel: "builder (box of ann)"})
	if err := h.resolved(protocol.Event{Kind: protocol.KindRequestResolved, Data: data}, &pumpState{}); err != nil {
		t.Fatal(err)
	}
	want := "builder (box of ann) dismissed this question without answering."
	if got.Status != "failed" || got.RawOutput != want || !reflect.DeepEqual(got.Content, []any{textItem(want)}) {
		t.Errorf("update = %+v, want failed with %q", got, want)
	}
}
