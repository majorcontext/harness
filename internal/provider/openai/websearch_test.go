package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/provider"
)

const searchItem = `{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"go release"}}`

func transcodedItems(t *testing.T, parts ...message.Part) []string {
	t.Helper()
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "find"}}},
		message.Message{Role: message.RoleAssistant, Parts: parts}))
	var items []string
	for _, raw := range out.Input[1:] {
		items = append(items, string(raw))
	}
	return items
}

func TestTranscodeWebSearchReplaysTheItemOfItsFamilyAndOthersAsText(t *testing.T) {
	own := &message.WebSearch{ID: "ws_1", Query: "go release", ProviderData: message.ProviderData{Family: json.RawMessage(searchItem)}}
	if got := transcodedItems(t, own, &message.Text{Text: "ok", Citations: []message.Citation{{URL: "https://go.dev"}}}); len(got) != 2 || got[0] != searchItem || strings.Contains(got[1], "go.dev") {
		t.Errorf("items = %v, want the stored item, then the text with no citations", got)
	}
	foreign := &message.WebSearch{ID: "srvtoolu_1", Query: "go release", Results: []message.SearchResult{{Title: "Go", URL: "https://go.dev"}},
		ProviderData: message.ProviderData{"anthropic": json.RawMessage(`{"content":[]}`)}}
	got := transcodedItems(t, foreign)
	if len(got) != 1 || !strings.Contains(got[0], `"role":"assistant"`) || !strings.Contains(got[0], "[web search: go release]\\n- Go \\u003chttps://go.dev\\u003e") {
		t.Errorf("items = %v, want one assistant message that holds the query and the source", got)
	}
}

func TestTranscodeOffersTheSearchToolAfterTheFunctionTools(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "find"}}})
	req.Tools = []provider.ToolDef{{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req.WebSearch = true
	raw, err := json.Marshal(mustTranscode(t, req).Tools)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"type":"function","name":"bash","parameters":{"type":"object"}},{"type":"web_search"}]`; string(raw) != want {
		t.Errorf("tools = %s, want %s", raw, want)
	}
}

func TestStreamOfARecordedSearchBecomesOnePartWithCitations(t *testing.T) {
	body, err := os.ReadFile("../../../testdata/wire/responses.websearch.sse")
	if err != nil {
		t.Fatal(err)
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
	s, err := c.Stream(context.Background(), baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "find"}}}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var done provider.Event
	for _, ev := range collect(t, s) {
		if ev.Type == provider.EventDone {
			done = ev
		}
	}
	var search *message.WebSearch
	var text *message.Text
	for _, p := range done.Message.Parts {
		switch p := p.(type) {
		case *message.WebSearch:
			search = p
		case *message.Text:
			text = p
		}
	}
	if search == nil || search.Query == "" || search.ID == "" {
		t.Fatalf("parts = %#v, want a search with a query and an ID", done.Message.Parts)
	}
	if raw, ok := search.ProviderData.Get(Family); !ok || !strings.Contains(string(raw), `"web_search_call"`) {
		t.Errorf("provider data = %s, want the item", raw)
	}
	if text == nil || len(text.Citations) == 0 || text.Citations[0].URL == "" || text.Citations[0].Title == "" {
		t.Errorf("text = %#v, want citations", text)
	}
}

func TestStreamOfAFailedSearchItemHoldsAnErrorAndNoReplayData(t *testing.T) {
	item := `{"id":"ws_1","type":"web_search_call","status":"failed","action":{"type":"search","query":"q"}}`
	body := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{}}}\n\n"
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
	s, err := c.Stream(context.Background(), baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "find"}}}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, ev := range collect(t, s) {
		if ev.Type != provider.EventDone {
			continue
		}
		if w, _ := ev.Message.Parts[0].(*message.WebSearch); w == nil || w.Error == "" || len(w.ProviderData) != 0 {
			t.Errorf("parts[0] = %#v, want a failed search with no provider data", ev.Message.Parts[0])
		}
	}
}
