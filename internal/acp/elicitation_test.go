package acp

import (
	"encoding/json"
	"reflect"
	"testing"
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
		"type": "object", "required": []string{"question_1", "question_2"},
		"properties": map[string]any{
			"question_1": map[string]any{"type": "array", "title": "Tools", "description": "Which tools?", "minItems": 1,
				"items": map[string]any{"type": "string", "enum": []string{"lint", "test"}}},
			"question_2": map[string]any{"type": "string", "title": "Name it?", "description": "Name it?"},
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
