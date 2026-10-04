package harness

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/protocol"
)

// Session is a session that this runtime runs.
type Session struct {
	a  *session.Actor
	r  *Runtime
	id string
	// recovered closes once Open has settled or opened each unsettled child.
	recovered chan struct{}
}

// View returns the session as of its last durable record.
func (s *Session) View() protocol.Session {
	synced := s.a.Synced()
	v := detach(s.a.View().Session)
	v.SyncedSeq = synced
	return v
}

func detach(s protocol.Session) protocol.Session {
	s.Queued = slices.Clone(s.Queued)
	if s.Goal != nil {
		g := *s.Goal
		s.Goal = &g
	}
	return s
}

// Submit admits an input. It starts a turn when none runs and queues the
// input otherwise. A steer input joins the running turn at its next item
// when the backend accepts steering. A repeated input ID returns the
// original receipt.
func (s *Session) Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error) {
	a, _, err := s.Admit(ctx, in)
	return a, err
}

// Admit is Submit that also reports whether in repeats an input that the
// session already admitted. The verdict is atomic with the admission.
func (s *Session) Admit(ctx context.Context, in protocol.Input) (protocol.Admitted, bool, error) {
	ev, err := admission(in)
	if err != nil {
		return protocol.Admitted{}, false, err
	}
	seq, repeat, err := s.a.Submit(ctx, ev, in.ExpectedTurnID)
	if err != nil {
		return protocol.Admitted{}, false, err
	}
	return protocol.Admitted{InputID: in.ID, Seq: seq}, repeat, nil
}

func admission(in protocol.Input) (eventlog.InputAdmitted, error) {
	ev := eventlog.InputAdmitted{InputID: in.ID, Delivery: eventlog.Delivery(in.Delivery), Source: in.Source}
	if ev.Delivery == "" {
		ev.Delivery = eventlog.DeliveryQueue
	}
	if ev.Source == "" {
		ev.Source = "user"
	}
	switch {
	case in.ID == "":
		return ev, fmt.Errorf("%w: input id is empty", ErrInvalidRequest)
	case len(in.Parts) == 0:
		return ev, fmt.Errorf("%w: input %s has no parts", ErrInvalidRequest, in.ID)
	case ev.Delivery != eventlog.DeliveryQueue && ev.Delivery != eventlog.DeliverySteer:
		return ev, fmt.Errorf("%w: input %s has delivery %q", ErrInvalidRequest, in.ID, in.Delivery)
	}
	for _, p := range in.Parts {
		if p.Type != protocol.PartText {
			return ev, fmt.Errorf("%w: input %s has part type %q", ErrInvalidRequest, in.ID, p.Type)
		}
		ev.Parts = append(ev.Parts, eventlog.Part{Type: eventlog.PartText, Text: p.Text})
	}
	return ev, nil
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
	return s.r.interruptTree(ctx, s.id, stop)
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

// Compact folds the turns before the newest compaction_keep_turns into a
// summary that the next model call reads first. A backend that owns its
// context runs its own /compact command instead. It returns when the
// compaction ends, and fails with ErrSessionBusy while a turn runs or
// inputs wait.
func (s *Session) Compact(ctx context.Context) error { return s.a.Compact(ctx) }

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
}

// OpenView reads a session from st without owning it: no Acquire, no appends.
func OpenView(ctx context.Context, st Store, id string) (*View, error) {
	s, err := session.Load(ctx, id, storeLog{st, id})
	if err != nil {
		return nil, err
	}
	return &View{st: st, id: id, state: session.Describe(id, s)}, nil
}

// Session returns the session as of OpenView.
func (v *View) Session() protocol.Session { return detach(v.state) }

// Events yields the events after seq and ends at the head that OpenView read.
func (v *View) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return session.Stored(ctx, storeLog{v.st, v.id}, after, v.state.HeadSeq)
}

// Update changes the settings of the session and returns its view. The next
// turn uses them; a running turn keeps its own until a handoff resumes it.
// A model that no configured provider serves fails with ErrModelUnavailable.
// A move to another provider fails with ErrInvalidRequest when either
// backend owns its context.
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
