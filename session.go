package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/admit"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
	"github.com/majorcontext/harness/protocol"
)

// Session is a session that this runtime runs.
type Session struct {
	a  *session.Actor
	r  *Runtime
	id string
	// root is the first ancestor of the session, or its own ID, and depth is
	// the number of its ancestors.
	root  string
	depth int
	// recovered closes once Open has settled or opened each unsettled child.
	recovered chan struct{}
}

// View returns the session as of its last durable record.
func (s *Session) View() protocol.Session {
	synced := s.a.Synced()
	v := detach(s.a.View().Session)
	v.SyncedSeq = synced
	v.Plugins = s.r.pluginInfo()
	return v
}

func detach(s protocol.Session) protocol.Session {
	s.Queued = slices.Clone(s.Queued)
	if s.Goal != nil {
		g := *s.Goal
		s.Goal = &g
	}
	if s.LastTurn != nil {
		l := *s.LastTurn
		s.LastTurn = &l
	}
	if s.SubscriptionUsage != nil {
		u := *s.SubscriptionUsage
		u.Windows = slices.Clone(u.Windows)
		if u.Overage != nil {
			o := *u.Overage
			u.Overage = &o
		}
		s.SubscriptionUsage = &u
	}
	return s
}

// Submit admits an input. It starts a turn when none runs. While a turn runs,
// a steer input, which is the default, joins it at its next item boundary,
// and a queue input waits for the next turn. A repeated input ID returns
// the original receipt with Repeat set; the verdict is atomic with the
// admission. A typed slash command records command.recorded instead of an
// input, and the receipt carries its status; see docs/architecture.md.
func (s *Session) Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error) {
	if _, err := admit.Provenance(in.SourceID, in.SourceLabel); err != nil {
		return protocol.Admitted{}, fmt.Errorf("%w: input %s: %w", ErrInvalidRequest, in.ID, err)
	}
	if in.Source == protocol.SourceTyped && in.ID != "" && len(in.Parts) == 1 && in.Parts[0].Type == protocol.PartText {
		p, next, err := s.resolve(in)
		if err != nil {
			return protocol.Admitted{}, err
		}
		if p != nil {
			return s.command(ctx, p)
		}
		in = next
	}
	ev, blobs, err := admit.Input(in)
	if err != nil {
		return protocol.Admitted{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	for _, b := range blobs {
		if err := s.r.store.PutBlob(ctx, s.id, b.Key, bytes.NewReader(b.Data)); err != nil {
			return protocol.Admitted{}, err
		}
	}
	seq, repeat, err := s.a.Submit(ctx, ev, in.ExpectedTurnID)
	if err != nil {
		return protocol.Admitted{}, err
	}
	return protocol.Admitted{InputID: in.ID, Seq: seq, Repeat: repeat}, nil
}

// Interrupt stops the running turn and returns after it has ended. The
// partial turn stays in the log, and the next queued input starts. With
// Tree, it then stops the turn of each descendant that this runtime runs
// and withdraws its queued inputs. A stopped descendant settles canceled
// with its parent, and starts no turn of a parent inside the tree.
func (s *Session) Interrupt(ctx context.Context, req protocol.Interrupt) error {
	stop := func(ctx context.Context) error { return s.a.Interrupt(ctx, req.TurnID) }
	if !req.Tree {
		return stop(ctx)
	}
	return s.r.tree.Interrupt(ctx, s.id, stop)
}

// Resolve answers or dismisses the open request requestID: with res.Answer, the
// answer of the user, or with res.Dismiss. An answer runs a turn with no input,
// which hands the answer to the backend that asked. The receipt holds the seq of
// the request.resolved record and says whether a turn started. A request that is not open
// fails with ErrRequestNotPending. For a question of Claude Code the request
// ID is the call ID of the AskUserQuestion item, and the answer maps each
// question to the chosen label or free text.
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) (protocol.Resolved, error) {
	if res.Dismiss && len(res.Answer) > 0 || !res.Dismiss && !hasAnswer(res.Answer) || len(res.Answer) > 0 && !json.Valid(res.Answer) {
		return protocol.Resolved{}, fmt.Errorf("%w: a resolution holds one answer, or a dismissal", ErrInvalidRequest)
	}
	got, err := s.a.Resolve(ctx, requestID, res.Answer, res.Dismiss)
	if errors.Is(err, session.ErrBadAnswer) {
		return protocol.Resolved{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return got, err
}

// hasAnswer reports whether answer is a JSON value other than null, an empty
// object, or an empty string, whatever whitespace surrounds it.
func hasAnswer(answer json.RawMessage) bool {
	var b bytes.Buffer
	if json.Compact(&b, answer) != nil {
		return len(answer) > 0
	}
	return !slices.Contains([]string{"", "null", "{}", `""`}, b.String())
}

// SetGoal replaces the goal of the session, as Claude Code /goal does. An
// evaluator judges each turn, and its guidance is the input of the next one.
func (s *Session) SetGoal(ctx context.Context, g protocol.Goal) error {
	if strings.TrimSpace(g.Condition) == "" || g.MaxTurns < 0 {
		return fmt.Errorf("%w: a goal needs a condition and max_turns >= 0", ErrInvalidRequest)
	}
	if s.r.evaluator == "" {
		return fmt.Errorf("%w: a goal needs goal_evaluator_model", ErrInvalidRequest)
	}
	return s.a.SetGoal(ctx, g.Condition, g.MaxTurns)
}

// ClearGoal clears the goal and returns after a running goal turn stops.
func (s *Session) ClearGoal(ctx context.Context) error { return s.a.ClearGoal(ctx) }

// Compact folds the turns before the newest req.KeepTurns, or
// compaction_keep_turns, into a summary that the next model call reads
// first. A backend that owns its context runs its own /compact command
// instead, and takes no KeepTurns. It returns when the compaction ends with
// what it folded, and fails with ErrSessionBusy while a turn runs or inputs
// wait. A session with too few turns folds nothing, with no error.
func (s *Session) Compact(ctx context.Context, req protocol.Compact) (protocol.Compacted, error) {
	keep := 0
	if req.KeepTurns != nil {
		if keep = *req.KeepTurns; keep < 1 {
			return protocol.Compacted{}, fmt.Errorf("%w: keep_turns must be >= 1", ErrInvalidRequest)
		}
	}
	c, ran, err := s.a.Compact(ctx, keep)
	if errors.Is(err, session.ErrKeepTurns) {
		err = fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err != nil || !ran {
		return protocol.Compacted{}, err
	}
	return protocol.Compacted{FromSeq: c.FromSeq, ToSeq: c.ToSeq, ByBackend: c.ByBackend, Folded: true}, nil
}

// Events yields the durable events after seq, then each new one as it is
// appended, with the ephemeral frames of the running turn between them:
// item.started, item.delta, and status. A slow reader can miss any frame,
// never a durable event, so the deltas of an item can have holes; its
// item.completed holds the whole item. It ends with ErrSessionNotOwned
// when the session stops here.
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return s.a.Events(ctx, after)
}

// Release suspends the running turn for the next owner, waits for Sync to
// acknowledge every record, and releases the ownership.
func (s *Session) Release(ctx context.Context) error { return s.a.Release(ctx) }

// View is a read-only session that no runtime needs to own.
type View struct {
	st    Store
	id    string
	state protocol.Session
	log   *eventlog.State
}

// OpenView reads a session from st without owning it: no Acquire, no appends.
func OpenView(ctx context.Context, st Store, id string) (*View, error) {
	s, err := session.Load(ctx, id, storeLog{st, id})
	if err != nil {
		return nil, err
	}
	window := 0
	if ref, err := message.ParseModelRef(s.Model()); err == nil {
		window, _ = modelmeta.ContextWindow(ref)
	}
	return &View{st: st, id: id, state: session.Describe(id, s, window), log: s}, nil
}

// Resumable reports whether Open of the session has work to resume.
func (v *View) Resumable() bool { return v.log.Resumable() }

// Messages returns the page of the conversation before seq before; 0 is the
// newest page. A limit above protocol.MaxMessageLimit fails with ErrInvalidRequest.
func (v *View) Messages(_ context.Context, before uint64, limit int) (protocol.MessagePage, error) {
	if err := checkMessageLimit(limit); err != nil {
		return protocol.MessagePage{}, err
	}
	return v.log.MessagePage(before, limit), nil
}

func checkMessageLimit(limit int) error {
	if limit < 0 || limit > protocol.MaxMessageLimit {
		return fmt.Errorf("%w: limit must be between 0 and %d", ErrInvalidRequest, protocol.MaxMessageLimit)
	}
	return nil
}

// Session returns the session as of OpenView.
func (v *View) Session() protocol.Session { return detach(v.state) }

// Events yields the events after seq and ends at the head that OpenView read.
func (v *View) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return session.Stored(ctx, storeLog{v.st, v.id}, after, v.state.HeadSeq)
}

// ReadEvents yields the stored events of session id after seq after, through
// the head that it reads first. It neither owns the session nor replays the
// log, so it equals View.Events only for a log that replays. A session with no
// log yields nothing; a record that does not decode, or whose envelope seq
// differs from its store seq, yields its error and ends.
func ReadEvents(ctx context.Context, st Store, id string, after uint64) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		head, err := st.Head(ctx, id)
		if err != nil {
			yield(protocol.Event{}, err)
			return
		}
		for e, err := range session.Stored(ctx, storeLog{st, id}, after, head) {
			if !yield(e, err) {
				return
			}
		}
	}
}

// Update changes the settings of the session and returns its view. A running
// turn that owns no loop uses them from its next model call; a backend that
// owns its loop uses them from its next run.
// A model that no configured provider serves fails with ErrModelUnavailable.
// A backend that owns its loop reads the history of another provider through
// the get_conversation_history tool, so any two models may follow each other.
func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error) {
	if p.Effort != nil {
		if _, err := message.ParseEffort(*p.Effort); err != nil {
			return protocol.Session{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
	}
	err := s.a.Update(ctx, eventlog.SettingsChanged{Model: p.Model, Effort: p.Effort, ServiceTier: p.ServiceTier})
	if err != nil {
		return protocol.Session{}, err
	}
	return s.View(), nil
}
