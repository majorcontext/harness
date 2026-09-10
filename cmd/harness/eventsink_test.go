package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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

// captureSinkBatch delivers one batch to a fake receiver and returns the exact
// request body bytes. The wire form is the contract, so the tests compare bytes
// and key sets rather than a re-decoded Go struct that would hide an omitted or
// a surplus field.
func captureSinkBatch(t *testing.T, generation string, batch server.EventBatch, reply string) ([]byte, int64) {
	t.Helper()
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		body = read
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(ts.Close)

	sink := newHTTPEventSink(&config.EventSinkSpec{URL: ts.URL, Generation: generation})
	applied, err := sink.Deliver(context.Background(), batch)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	return body, applied
}

// TestHTTPEventSinkOmitsFilteredForAnUnfilteredBatch pins wire compatibility
// for a receiver written before the selector existed. Input: an unfiltered
// EventBatch. Wrong output: a request body that carries a "filtered" key at
// all, which a strict receiver rejects as an unknown field.
func TestHTTPEventSinkOmitsFilteredForAnUnfilteredBatch(t *testing.T) {
	body, applied := captureSinkBatch(t, "jrnl_test", server.EventBatch{
		FromSeq: 6,
		ToSeq:   7,
		Records: []server.Event{{Type: "session.status", SessionID: "ses_1", Seq: 6}},
	}, `{"applied_through": 7}`)
	if applied != 7 {
		t.Errorf("appliedThrough = %d, want 7", applied)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode request body %s: %v", body, err)
	}
	keys := slices.Sorted(maps.Keys(got))
	want := []string{"from_seq", "generation", "records", "to_seq"}
	if !slices.Equal(keys, want) {
		t.Errorf("request keys = %v, want %v; body = %s", keys, want, body)
	}
}

// TestHTTPEventSinkEncodesAnEmptyFilteredCheckpoint pins the sparse-range
// contract. Input: a filtered EventBatch that scanned seqs 8 through 12 and
// selected nothing. Wrong output: a body without "filtered":true, or one whose
// "records" is null, either of which stops the receiver from acknowledging
// through to_seq and stalls the cursor at 7 forever.
func TestHTTPEventSinkEncodesAnEmptyFilteredCheckpoint(t *testing.T) {
	body, applied := captureSinkBatch(t, "jrnl_test", server.EventBatch{
		FromSeq:  8,
		ToSeq:    12,
		Filtered: true,
	}, `{"applied_through": 12}`)

	const want = `{"generation":"jrnl_test","from_seq":8,"to_seq":12,"filtered":true,"records":[]}`
	if string(body) != want {
		t.Errorf("request body =\n\t%s\nwant\n\t%s", body, want)
	}
	if applied != 12 {
		t.Errorf("appliedThrough = %d, want 12", applied)
	}
}
