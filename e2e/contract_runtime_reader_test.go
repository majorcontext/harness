package e2e

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestContractRuntimeReader(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a_running_and_a_stored_session_read_alike", rowARunningAndAStoredSessionReadAlike},
		{"a_blob_key_that_no_input_names_fails_with_blob_not_found", rowABlobKeyThatNoInputNamesFailsWithBlobNotFound},
		{"a_message_limit_over_the_maximum_fails_with_invalid_request", rowAMessageLimitOverTheMaximumFailsWithInvalidRequest},
		{"withdraw_of_an_input_that_is_not_queued_is_no_error", rowWithdrawOfAnInputThatIsNotQueuedIsNoError},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.run(t)
		})
	}
}

func readerRuntime(t *testing.T) (*harness.Runtime, *harness.Session, []byte) {
	t.Helper()
	t.Setenv("HARNESS_E2E_READER_KEY", "k")
	fake := harnesstest.NewChat(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}})
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: os.Getenv, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_READER_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	pdf := rowAttachments()[1].data
	in := protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "read it"}, {Type: protocol.PartBlob, MediaType: "application/pdf", Data: pdf}}}
	if _, err := s.Prompt(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	return r, s, pdf
}

func attachmentKey(t *testing.T, rd harness.Reader) string {
	t.Helper()
	page, err := rd.Messages(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page.Messages {
		for _, p := range m.Parts {
			if p.Key != "" {
				return p.Key
			}
		}
	}
	t.Fatal("no blob part in the conversation")
	return ""
}

func rowARunningAndAStoredSessionReadAlike(t *testing.T) {
	r, s, pdf := readerRuntime(t)
	live, err := r.Read(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if live != harness.Reader(s) {
		t.Errorf("Read of a running session = %T, want the Session", live)
	}
	key := attachmentKey(t, live)
	check := func(name string, rd harness.Reader) {
		t.Helper()
		if rd.Snapshot().ID != "s1" {
			t.Errorf("%s: Snapshot ID = %q, want s1", name, rd.Snapshot().ID)
		}
		b, err := rd.Blob(t.Context(), key)
		if err != nil {
			t.Fatalf("%s: Blob: %v", name, err)
		}
		defer func() { _ = b.Body.Close() }()
		got, _ := io.ReadAll(b.Body)
		if b.MediaType != "application/pdf" || b.Size != int64(len(pdf)) || string(got) != string(pdf) {
			t.Errorf("%s: Blob = %q %d %q, want the attachment", name, b.MediaType, b.Size, got)
		}
		if in, err := rd.Inputs(t.Context()); err != nil || len(in) != 0 {
			t.Errorf("%s: Inputs = %v, %v; want none", name, in, err)
		}
	}
	check("running", live)
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := r.Read(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.(*harness.View); !ok {
		t.Fatalf("Read of a stored session = %T, want a View", stored)
	}
	check("stored", stored)
}

func rowABlobKeyThatNoInputNamesFailsWithBlobNotFound(t *testing.T) {
	r, s, _ := readerRuntime(t)
	if _, err := s.Blob(t.Context(), "attachment-nope"); !errors.Is(err, harness.ErrBlobNotFound) {
		t.Errorf("Session.Blob = %v, want ErrBlobNotFound", err)
	}
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := r.Read(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Blob(t.Context(), "attachment-nope"); !errors.Is(err, harness.ErrBlobNotFound) {
		t.Errorf("View.Blob = %v, want ErrBlobNotFound", err)
	}
}

func rowAMessageLimitOverTheMaximumFailsWithInvalidRequest(t *testing.T) {
	r, s, _ := readerRuntime(t)
	if _, err := s.Messages(t.Context(), 0, protocol.MaxMessageLimit+1); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("Session.Messages = %v, want ErrInvalidRequest", err)
	}
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := r.Read(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Messages(t.Context(), 0, protocol.MaxMessageLimit+1); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("View.Messages = %v, want ErrInvalidRequest", err)
	}
}

func rowWithdrawOfAnInputThatIsNotQueuedIsNoError(t *testing.T) {
	_, s, _ := readerRuntime(t)
	if err := s.Withdraw(t.Context(), "never-queued"); err != nil {
		t.Errorf("Withdraw = %v, want nil", err)
	}
}
