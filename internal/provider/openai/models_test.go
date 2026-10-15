package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
