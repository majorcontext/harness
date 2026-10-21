package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestAccountClientSharesTheUnqualifiedLiveCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"slug":"full-b","visibility":"list","use_responses_lite":false,"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`))
	}))
	defer srv.Close()
	base := &Client{BaseURL: srv.URL, APIKey: "k", LiteUntilListed: true}
	account := &Client{LiteUntilListed: true}
	account.ShareLiveCatalog(base)
	if account.NativeWebSearch("full-b") {
		t.Fatal("an unlisted account model offered search before the base catalog loaded")
	}
	if _, err := base.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !account.NativeWebSearch("full-b") {
		t.Fatal("account model did not see the full model in the shared catalog")
	}
	if levels, ok := account.EffortControl("full-b"); !ok || !slices.Equal(levels.Levels, []string{"low", "high"}) {
		t.Fatalf("account model effort control = %+v, %v", levels, ok)
	}
}

func TestNativeWebSearchFollowsTheResponsesLiteFlagOfTheModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"slug":"lite-a","use_responses_lite":true},{"slug":"full-b","use_responses_lite":false}]}`))
	}))
	defer srv.Close()
	for _, untilListed := range []bool{false, true} {
		c := &Client{BaseURL: srv.URL, APIKey: "k", LiteUntilListed: untilListed}
		if got := c.NativeWebSearch("full-b"); got == untilListed {
			t.Errorf("untilListed %v: search before the list = %v", untilListed, got)
		}
		if _, err := c.ListModels(context.Background()); err != nil {
			t.Fatal(err)
		}
		for model, want := range map[string]bool{"lite-a": false, "full-b": true, "unlisted": !untilListed} {
			if got := c.NativeWebSearch(model); got != want {
				t.Errorf("untilListed %v: NativeWebSearch(%q) = %v, want %v", untilListed, model, got, want)
			}
		}
	}
}
