package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

var bg = context.Background()

const (
	banner      = "[compacted summary of earlier conversation]\n\n"
	instruction = "Summarize the conversation above, following the system prompt's instructions."
)

func answer(in string, tokens int) harnesstest.Step {
	return harnesstest.Step{Name: in, Match: harnesstest.LastUserText(in),
		Reply: harnesstest.Reply{Text: "re " + in, Usage: harnesstest.Usage{Input: tokens, Output: 1}}}
}

func summarize(name string, rep harnesstest.Reply) harnesstest.Step {
	return harnesstest.Step{Name: name, Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: rep}
}

// holdNth holds request n until its ctx ends, and closes held when it arrives.
type holdNth struct {
	n    int32
	seen *atomic.Int32
	held chan struct{}
}

func (h holdNth) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.seen.Add(1) == h.n {
		close(h.held)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(req)
}

// open runs session s1 on codex/gpt-5, a 400000-token window, served by s,
// with the compaction settings threshold and keep. It creates s1 when st has no log.
func open(t *testing.T, s *harnesstest.OpenAI, st harness.Store, threshold float64, keep int, rt http.RoundTripper) (*harness.Runtime, *harness.Session) {
	t.Helper()
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: s.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: st, ModelTransport: func(string) http.RoundTripper { return rt },
		Config: config.Config{PromptRetries: &retries, CompactionThreshold: threshold, CompactionKeepTurns: keep,
			Providers: map[string]config.Provider{"codex": p}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(bg) })
	sess, err := r.Open(bg, "s1")
	if err != nil {
		sess, err = r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, sess
}

// converse submits each text as an input with the text as its ID, and
// waits for the turn that it starts to end.
func converse(t *testing.T, s *harness.Session, texts ...string) {
	t.Helper()
	for _, txt := range texts {
		after := s.View().HeadSeq
		if _, err := s.Submit(bg, protocol.Input{ID: txt, Parts: []protocol.Part{{Type: protocol.PartText, Text: txt}}}); err != nil {
			t.Fatal(err)
		}
		await(t, s, after)
	}
}

// await waits for a turn.ended record after seq after.
func await(t *testing.T, s *harness.Session, after uint64) {
	t.Helper()
	for e, err := range s.Events(bg, after) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			return
		}
	}
}

func transcript(req harnesstest.Request) []string {
	var out []string
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			out = append(out, m.Role+" "+p.Text)
		}
	}
	return out
}

func kinds(t *testing.T, st harness.Store, after uint64) []string {
	t.Helper()
	recs, err := st.Read(bg, "s1", after, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		var env struct{ K string }
		_ = json.Unmarshal(r.Data, &env)
		out = append(out, env.K)
	}
	return out
}

func TestCompactFoldsTheOlderTurns(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, answer("alpha", 5), answer("bravo", 5), answer("charlie", 5),
		summarize("summary", harnesstest.Reply{Text: "sum"}), answer("delta", 5))
	_, sess := open(t, s, harness.NewMemStore(), 0, 0, nil)
	converse(t, sess, "alpha", "bravo", "charlie")
	if err := sess.Compact(bg); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	converse(t, sess, "delta")
	reqs := s.Requests()
	if got, want := transcript(reqs[3]), []string{"user alpha", "assistant re alpha", "user " + instruction}; !slices.Equal(got, want) {
		t.Errorf("summary request = %q, want %q", got, want)
	}
	want := []string{"user " + banner + "sum", "user bravo", "assistant re bravo", "user charlie", "assistant re charlie", "user delta"}
	if got := transcript(reqs[4]); !slices.Equal(got, want) {
		t.Errorf("request after Compact = %q, want %q", got, want)
	}
}

func TestAutoCompaction(t *testing.T) {
	full := []string{"user alpha", "assistant re alpha", "user bravo", "assistant re bravo", "user charlie"}
	for _, tc := range []struct {
		name      string
		threshold float64
		tokens    int
		summary   *harnesstest.Reply
		want      []string
	}{
		{"a reading under the threshold does not compact", 0.5, 199_999, nil, full},
		{"a reading at the threshold compacts before the next turn", 0.5, 200_000, &harnesstest.Reply{Text: "sum"},
			[]string{"user " + banner + "sum", "user bravo", "assistant re bravo", "user charlie"}},
		{"a failed summary keeps the history and the turn runs", 0.5, 200_000, &harnesstest.Reply{}, full},
		{"a negative threshold is the default threshold", -1, 200_000, nil, full},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []harnesstest.Step{answer("alpha", 5), answer("bravo", tc.tokens), answer("charlie", 5)}
			if tc.summary != nil {
				steps = append(steps, summarize("summary", *tc.summary))
			}
			s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
			_, sess := open(t, s, harness.NewMemStore(), tc.threshold, 1, nil)
			converse(t, sess, "alpha", "bravo", "charlie")
			reqs := s.Requests()
			if got := transcript(reqs[len(reqs)-1]); !slices.Equal(got, tc.want) {
				t.Errorf("last request = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandoffDuringCompaction(t *testing.T) {
	charlie := protocol.Input{ID: "charlie", Parts: []protocol.Part{{Type: protocol.PartText, Text: "charlie"}}}
	for _, tc := range []struct {
		name      string
		threshold float64
		bravo     int
		before    []string
		start     func(*harness.Session) error
		held      []string
		next      func(*testing.T, *harness.Session, uint64) error
		want      []string
	}{
		{"a manual compaction stops and the next owner compacts", 0, 5, []string{"alpha", "bravo", "charlie"},
			func(s *harness.Session) error {
				if err := s.Compact(bg); err == nil {
					return errors.New("Compact during a handoff = nil, want an error")
				}
				return nil
			},
			nil,
			func(_ *testing.T, s *harness.Session, _ uint64) error { return s.Compact(bg) },
			[]string{"owner.acquired", "compaction.applied"}},
		{"an auto-compaction stops and the next owner runs the queued input", 0.5, 200_000, []string{"alpha", "bravo"},
			func(s *harness.Session) error { _, err := s.Submit(bg, charlie); return err },
			[]string{"input.admitted"},
			func(t *testing.T, s *harness.Session, head uint64) error { await(t, s, head); return nil },
			[]string{"input.admitted", "owner.acquired", "compaction.applied", "turn.started", "context.measured", "item.completed", "turn.ended"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, answer("alpha", 5), answer("bravo", tc.bravo), answer("charlie", 5),
				summarize("summary", harnesstest.Reply{Text: "sum"}))
			st := harness.NewMemStore()
			hold := holdNth{n: int32(len(tc.before)) + 1, seen: new(atomic.Int32), held: make(chan struct{})}
			r1, sess := open(t, s, st, tc.threshold, 1, hold)
			converse(t, sess, tc.before...)
			head := sess.View().HeadSeq
			errc := make(chan error, 1)
			go func() { errc <- tc.start(sess) }()
			<-hold.held
			if err := r1.Close(bg); err != nil {
				t.Fatal(err)
			}
			if err := <-errc; err != nil {
				t.Fatal(err)
			}
			if got := kinds(t, st, head); !slices.Equal(got, tc.held) {
				t.Fatalf("records after the handoff = %q, want %q", got, tc.held)
			}
			_, next := open(t, s, st, tc.threshold, 1, nil)
			if err := tc.next(t, next, head); err != nil {
				t.Fatalf("next owner: %v", err)
			}
			if got := kinds(t, st, head); !slices.Equal(got, tc.want) {
				t.Errorf("records after the handoff = %q, want %q", got, tc.want)
			}
		})
	}
}
