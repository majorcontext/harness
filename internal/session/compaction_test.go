package session_test

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

func compactAnswer(in string, tokens int) harnesstest.Step {
	return harnesstest.Step{Name: in, Match: harnesstest.LastUserText(in),
		Reply: harnesstest.Reply{Text: "re " + in, Usage: harnesstest.Usage{Input: tokens, Output: 1}}}
}

func compactSummary(name string, rep harnesstest.Reply) harnesstest.Step {
	return harnesstest.Step{Name: name, Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: rep}
}

// holdNth holds request n until its ctx ends, and closes held when it arrives.
// With finish, it then serves the request, as a response that wins the race
// with the handoff.
type holdNth struct {
	n      int32
	seen   *atomic.Int32
	held   chan struct{}
	finish bool
}

func (h holdNth) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.seen.Add(1) == h.n {
		close(h.held)
		<-req.Context().Done()
		if h.finish {
			return http.DefaultTransport.RoundTrip(req.Clone(context.WithoutCancel(req.Context())))
		}
		return nil, req.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(req)
}

// openCompacting runs session s1 on codex/gpt-5, a 400000-token window, served by s,
// with the compaction setting keep. It creates s1 when st has no log.
func openCompacting(t *testing.T, s *harnesstest.OpenAI, st harness.Store, keep int, rt http.RoundTripper) (*harness.Runtime, *harness.Session) {
	t.Helper()
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: s.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: st, ModelTransport: func(string) http.RoundTripper { return rt },
		Config: config.Config{PromptRetries: &retries, CompactionKeepTurns: keep,
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
func ask(t *testing.T, s *harness.Session, texts ...string) {
	t.Helper()
	for _, txt := range texts {
		after := s.View().HeadSeq
		if _, err := s.Submit(bg, protocol.Input{ID: txt, Parts: []protocol.Part{{Type: protocol.PartText, Text: txt}}}); err != nil {
			t.Fatal(err)
		}
		awaitTurn(t, s, after)
	}
}

// await waits for a turn.ended record after seq after.
func awaitTurn(t *testing.T, s *harness.Session, after uint64) {
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

func recordKinds(t *testing.T, st harness.Store, after uint64) []string {
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

func TestHandoffDuringCompaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish bool
		held   []string
		want   []string
	}{
		{"a manual compaction stops and the next owner compacts", false,
			nil, []string{"owner.acquired", "compaction.applied"}},
		{"a summary that finishes during the handoff records its usage and no compaction", true,
			[]string{"context.measured"}, []string{"context.measured", "owner.acquired", "compaction.applied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []harnesstest.Step{compactAnswer("alpha", 5), compactAnswer("bravo", 5), compactAnswer("charlie", 5), compactSummary("summary", harnesstest.Reply{Text: "sum"})}
			if tc.finish {
				steps = append(steps, compactSummary("summary again", harnesstest.Reply{Text: "sum"}))
			}
			s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
			st := harness.NewMemStore()
			hold := holdNth{n: 4, seen: new(atomic.Int32), held: make(chan struct{}), finish: tc.finish}
			r1, sess := openCompacting(t, s, st, 1, hold)
			ask(t, sess, "alpha", "bravo", "charlie")
			head := sess.View().HeadSeq
			errc := make(chan error, 1)
			go func() {
				if _, err := sess.Compact(bg, protocol.Compact{}); err == nil {
					errc <- errors.New("Compact during a handoff = nil, want an error")
					return
				}
				errc <- nil
			}()
			<-hold.held
			if err := r1.Close(bg); err != nil {
				t.Fatal(err)
			}
			if err := <-errc; err != nil {
				t.Fatal(err)
			}
			if got := recordKinds(t, st, head); !slices.Equal(got, tc.held) {
				t.Fatalf("records after the handoff = %q, want %q", got, tc.held)
			}
			_, next := openCompacting(t, s, st, 1, nil)
			if _, err := next.Compact(bg, protocol.Compact{}); err != nil {
				t.Fatalf("next owner: %v", err)
			}
			if got := recordKinds(t, st, head); !slices.Equal(got, tc.want) {
				t.Errorf("records after the handoff = %q, want %q", got, tc.want)
			}
		})
	}
}
