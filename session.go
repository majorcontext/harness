package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"slices"
	"strings"
	"unicode"

	"github.com/majorcontext/harness/internal/admit"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/storecheck"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// Session is a shared handle on a running session; it stops for good at Done.
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

// ID returns the session ID.
func (s *Session) ID() string { return s.id }

// Done closes when the session stops here, after its Ownership is released.
func (s *Session) Done() <-chan struct{} { return s.a.Done() }

// Err is nil before Done, then the stop cause; see ErrSessionReleased. An error of the Store or a Sync rejection that stops the session matches ErrSessionNotOwned and that error, and none of the three causes.
func (s *Session) Err() error { return s.a.Err() }

// Snapshot returns the session as of its last durable record.
func (s *Session) Snapshot() protocol.Session {
	synced := s.a.Synced()
	v := detach(s.a.View().Session)
	v.SyncedSeq = synced
	v.Plugins = s.r.pluginInfo()
	return v
}

func detach(s protocol.Session) protocol.Session {
	s.Queued = slices.Clone(s.Queued)
	s.AllowedTools = slices.Clone(s.AllowedTools)
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

// Prompt admits an input and returns the Turn that will take it. It starts a
// turn when none runs. While a turn runs, a steer input, which is the default,
// joins it at its next item boundary, and a queue input waits for the next
// turn. A repeated input ID returns the Turn of the original receipt, with
// Repeat set; the verdict is atomic with the admission. A typed slash command
// records command.recorded instead of an input, and the receipt carries its
// status; see docs/architecture.md, Inputs.
func (s *Session) Prompt(ctx context.Context, in protocol.Input) (*Turn, error) {
	adm, err := s.admitInput(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.Turn(adm), nil
}

// Turn returns the Turn of the receipt a, for example after Open on another
// runtime. It reads nothing until a method of the Turn runs.
func (s *Session) Turn(a protocol.Admitted) *Turn { return &Turn{s: s, adm: a} }

func (s *Session) admitInput(ctx context.Context, in protocol.Input) (protocol.Admitted, error) {
	if _, err := admit.Provenance(in.SourceID, in.SourceLabel); err != nil {
		return protocol.Admitted{}, invalidInput(in.ID, err)
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
		return protocol.Admitted{}, invalidInput(in.ID, err)
	}
	if err := checkImageModel(s.Snapshot().Model, in); err != nil {
		return protocol.Admitted{}, err
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

// CheckInput runs the checks that Submit runs on an input before it stores
// anything, and stores nothing. It returns the error that Submit returns for
// the same input, or nil. An empty model skips the check of image input.
func CheckInput(model string, in protocol.Input) error {
	if _, err := admit.Provenance(in.SourceID, in.SourceLabel); err != nil {
		return invalidInput(in.ID, err)
	}
	if _, _, err := admit.Input(in); err != nil {
		return invalidInput(in.ID, err)
	}
	return checkImageModel(model, in)
}

// checkImageModel refuses an image attachment for a model that the model
// facts list without image input. A model that the facts lack passes.
func checkImageModel(model string, in protocol.Input) error {
	if !slices.ContainsFunc(in.Parts, func(p protocol.Part) bool {
		return p.Type == protocol.PartBlob && strings.HasPrefix(p.MediaType, "image/")
	}) {
		return nil
	}
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return nil
	}
	if accepts, known := modelmeta.ImageInput(ref); known && !accepts {
		return invalidInput(in.ID, fmt.Errorf("the model %s does not accept image input", model))
	}
	return nil
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
// ID is the call ID of the question item, and the answer maps each
// question to the chosen label or free text. res.Source, res.SourceID, and
// res.SourceLabel name who resolved it, with the checks of an Input.
func (s *Session) Resolve(ctx context.Context, requestID string, res protocol.Resolution) (protocol.Resolved, error) {
	if strings.TrimSpace(res.Response) == "" {
		res.Response = ""
	}
	if err := res.Validate(); err != nil {
		return protocol.Resolved{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	label, err := admit.Provenance(res.SourceID, res.SourceLabel)
	if err != nil {
		return protocol.Resolved{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	res.SourceLabel = label
	got, err := s.a.Resolve(ctx, requestID, res)
	if errors.Is(err, session.ErrBadAnswer) {
		return protocol.Resolved{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return got, err
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
// item.completed holds the whole item. When the session stops here, it ends
// with Err, which matches ErrSessionNotOwned.
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return s.a.Events(ctx, after)
}

// Release suspends the running turn for the next owner, waits for Sync to
// acknowledge every record, and releases the ownership.
func (s *Session) Release(ctx context.Context) error { return s.a.Release(ctx) }

// Messages returns the page of the conversation before seq before; 0 is the
// newest page. A limit above protocol.MaxMessageLimit fails with ErrInvalidRequest.
func (s *Session) Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error) {
	if err := checkMessageLimit(limit); err != nil {
		return protocol.MessagePage{}, err
	}
	var page protocol.MessagePage
	err := s.a.Read(ctx, func(st *eventlog.State) { page = st.MessagePage(before, limit) })
	return page, err
}

// Inputs returns the inputs that no turn has taken, oldest first.
func (s *Session) Inputs(ctx context.Context) ([]protocol.QueuedInput, error) {
	var in []protocol.QueuedInput
	err := s.a.Read(ctx, func(st *eventlog.State) { in = st.QueuedInputs() })
	return in, err
}

// Blob returns the attachment that a blob part of an input of the session
// names by key. Any other key fails with ErrBlobNotFound.
func (s *Session) Blob(ctx context.Context, key string) (Blob, error) {
	var mediaType string
	var size int
	var ok bool
	if err := s.a.Read(ctx, func(st *eventlog.State) { mediaType, size, ok = st.Attachment(key) }); err != nil {
		return Blob{}, err
	}
	return attachment(ctx, s.r.store, s.id, key, mediaType, size, ok)
}

// Withdraw removes the queued input inputID. An input that is not queued is
// no error.
func (s *Session) Withdraw(ctx context.Context, inputID string) error {
	return s.a.Withdraw(ctx, inputID)
}

var (
	_ Reader = (*Session)(nil)
	_ Reader = (*View)(nil)
)

// Read returns the running Session of id, or a View of its stored log. It
// acquires nothing and appends nothing. A View names the plugins of r.
func (r *Runtime) Read(ctx context.Context, id string) (Reader, error) {
	if s := r.running(id); s != nil {
		return s, nil
	}
	v, err := openView(ctx, r.store, id, r.models.Capabilities)
	if err != nil {
		return nil, err
	}
	v.plugins = r.pluginInfo()
	return v, nil
}

// Reader is the read surface of a session, live or stored. *Session and *View
// implement it. Messages, Inputs, and Blob fail with the errors of their
// methods; Events ends with Err for a Session that stops.
type Reader interface {
	Snapshot() protocol.Session
	Messages(ctx context.Context, before uint64, limit int) (protocol.MessagePage, error)
	Inputs(ctx context.Context) ([]protocol.QueuedInput, error)
	Blob(ctx context.Context, key string) (Blob, error)
	Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error]
}

// Blob is an attachment. The caller closes Body.
type Blob struct {
	MediaType string
	Size      int64
	Body      io.ReadCloser
}

func attachment(ctx context.Context, st Store, id, key, mediaType string, size int, ok bool) (Blob, error) {
	if !ok {
		return Blob{}, fmt.Errorf("%w: %s", ErrBlobNotFound, key)
	}
	body, err := st.GetBlob(ctx, id, key)
	if errors.Is(err, fs.ErrNotExist) {
		return Blob{}, fmt.Errorf("%w: %s", ErrBlobNotFound, key)
	}
	return Blob{MediaType: mediaType, Size: int64(size), Body: body}, err
}

// View is a session read from a Store with no owner. It is immutable.
type View struct {
	st    Store
	id    string
	state protocol.Session
	log   *eventlog.State
	// plugins are those of the runtime that read the view; the log holds none.
	plugins []protocol.Plugin
}

// ID returns the session ID.
func (v *View) ID() string { return v.id }

// OpenView reads a session from st without owning it: no Acquire, no appends. An ID that a Store must refuse fails with ErrInvalidRequest.
// It has no backend, so the gauge window is the window of the newest reading.
func OpenView(ctx context.Context, st Store, id string) (*View, error) {
	return openView(ctx, st, id, nil)
}

// openView is OpenView with a resolver of the window of the session model,
// which a runtime supplies from its backends.
func openView(ctx context.Context, st Store, id string, windows func(model string) turn.Capabilities) (*View, error) {
	if err := storecheck.Name(id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	s, err := session.Load(ctx, id, storeLog{st, id})
	if err != nil {
		return nil, err
	}
	var caps turn.Capabilities
	if windows != nil {
		caps = windows(s.Model())
	}
	return &View{st: st, id: id, state: session.Describe(id, s, caps.ContextWindow, caps.WindowEstimated), log: s}, nil
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

// Snapshot returns the session as of OpenView.
func (v *View) Snapshot() protocol.Session {
	s := detach(v.state)
	s.Plugins = v.plugins
	return s
}

// Inputs returns the inputs that no turn has taken, oldest first, as of OpenView.
func (v *View) Inputs(context.Context) ([]protocol.QueuedInput, error) {
	return v.log.QueuedInputs(), nil
}

// Blob returns the attachment that a blob part of an input of the session
// names by key. Any other key fails with ErrBlobNotFound.
func (v *View) Blob(ctx context.Context, key string) (Blob, error) {
	mediaType, size, ok := v.log.Attachment(key)
	return attachment(ctx, v.st, v.id, key, mediaType, size, ok)
}

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

// checkEffort fails with ErrInvalidRequest when effort is not an effort level
// or empty. Create and Update share it.
func checkEffort(effort string) error {
	if _, err := modelref.ParseEffort(effort); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// maxTitleBytes is the longest title that a rename takes.
const maxTitleBytes = 200

// Update changes the settings of the session and returns its view. A running
// turn that owns no loop uses them from its next model call; a backend that
// owns its loop uses them from its next run.
// A model that no configured provider serves fails with ErrModelUnavailable.
// A backend that owns its loop reads the history of another provider through
// the get_conversation_history tool, so any two models may follow each other.
func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error) {
	if p.Effort != nil {
		if err := checkEffort(*p.Effort); err != nil {
			return protocol.Session{}, err
		}
	}
	title := p.Title
	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" || len(t) > maxTitleBytes || strings.ContainsFunc(t, unicode.IsControl) {
			return protocol.Session{}, fmt.Errorf("%w: title must be 1 to %d bytes with no control character", ErrInvalidRequest, maxTitleBytes)
		}
		title = &t
	}
	err := s.a.Update(ctx, protocol.SettingsChanged{Model: p.Model, Effort: p.Effort, ServiceTier: p.ServiceTier, Accounts: p.Accounts}, title)
	if err != nil {
		return protocol.Session{}, err
	}
	return s.Snapshot(), nil
}

// Advance applies the durable event e, which follows p.Seq, to p and returns the new Progress; p is unchanged.
// It is the one incremental fold of status, queue, goal, and last turn, and the session state uses it.
// An ephemeral frame or an unknown kind fails with protocol.ErrUnknownKind, and an event out of order with protocol.ErrSeq.
// Advance assumes a legal log.
func Advance(p protocol.Progress, e protocol.Event) (protocol.Progress, error) {
	return eventlog.Advance(p, e)
}
