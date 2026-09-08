package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/server"
)

func TestHTTPEventSinkPostsBatchAndReadsCursor(t *testing.T) {
	type wire struct {
		Generation string            `json:"generation"`
		FromSeq    int64             `json:"from_seq"`
		ToSeq      int64             `json:"to_seq"`
		Records    []json.RawMessage `json:"records"`
	}
	var got wire
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"applied_through": 7}`))
	}))
	t.Cleanup(ts.Close)

	sink := newHTTPEventSink(&config.EventSinkSpec{
		URL:        ts.URL,
		Headers:    map[string]string{"Authorization": "Bearer t"},
		Generation: "jrnl_abc",
	})

	applied, err := sink.Deliver(context.Background(), server.EventBatch{
		FromSeq: 6,
		ToSeq:   7,
		Records: []server.Event{{Type: "session.status", SessionID: "ses_1", Seq: 6}},
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if applied != 7 {
		t.Errorf("appliedThrough = %d, want 7", applied)
	}
	if auth != "Bearer t" {
		t.Errorf("Authorization = %q, want the configured header", auth)
	}
	if got.Generation != "jrnl_abc" || got.FromSeq != 6 || got.ToSeq != 7 || len(got.Records) != 1 {
		t.Errorf("body = %+v", got)
	}
}

func TestHTTPEventSinkRejectsNon2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"receiver_unavailable","message":"secret diagnostic detail"}`))
	}))
	t.Cleanup(ts.Close)

	sinkURL := ts.URL + "/sink?token=secret_query#secret_fragment"
	sink := newHTTPEventSink(&config.EventSinkSpec{URL: sinkURL})
	_, err := sink.Deliver(context.Background(), server.EventBatch{FromSeq: 1, ToSeq: 1})
	if err == nil {
		t.Fatal("Deliver succeeded on a 500, want an error so the cursor does not advance")
	}
	if !strings.Contains(err.Error(), "receiver_unavailable") {
		t.Errorf("error = %q, want sanitized receiver code", err)
	}
	if strings.Contains(err.Error(), "secret diagnostic detail") {
		t.Errorf("error includes untrusted response message: %q", err)
	}
	if strings.Contains(err.Error(), "secret_query") || strings.Contains(err.Error(), "secret_fragment") {
		t.Errorf("error includes configured URL secrets: %q", err)
	}
}
