package eventlog

import (
	"fmt"
	"slices"
	"time"

	"github.com/majorcontext/harness/protocol"
)

type lightEntry struct {
	Entry   entry
	Summary uint64
	At      time.Time
}

type attachmentInfo struct {
	MediaType string
	Size      int
}

// LightState folds a log without retaining historical message bodies. It is not safe for concurrent use.
type LightState struct {
	state       State
	folded      []lightEntry
	attachments map[string]attachmentInfo
	asked       map[string]uint64
	resolved    map[string]uint64
	commands    map[string]uint64
	retained    []uint64
}

// NewLightState returns an empty light fold.
func NewLightState() *LightState {
	l := &LightState{attachments: map[string]attachmentInfo{}, asked: map[string]uint64{}, resolved: map[string]uint64{}, commands: map[string]uint64{}}
	l.state.light = l
	return l
}

// Apply applies a record to the light fold.
func (l *LightState) Apply(r Record) error {
	env, err := Decode(r.Data)
	if err != nil {
		return err
	}
	if env.Seq != r.Seq {
		return fmt.Errorf("%w: record %d holds envelope seq %d", ErrSeq, r.Seq, env.Seq)
	}
	if err := l.state.apply(env); err != nil {
		return err
	}
	l.trim(env)
	return nil
}

func (l *LightState) trim(env Envelope) {
	s := &l.state
	switch e := env.Event.(type) {
	case protocol.InputAdmitted:
		for _, p := range e.Parts {
			if p.Type == protocol.LogPartBlob {
				l.attachments[p.BlobKey] = attachmentInfo{p.MediaType, p.Bytes}
			}
		}
	case protocol.InputWithdrawn:
		l.trimInput(e.InputID)
	case protocol.TurnStarted:
		for i := s.turnAt; i < len(s.history); i++ {
			h := &s.history[i]
			if h.seq != env.Seq || len(h.sources) > 0 {
				continue
			}
			if h.pinned {
				for _, id := range e.InputIDs {
					if IsReport(s.inputs[id].event.Parts) {
						h.sources = append(h.sources, s.inputs[id].seq)
					}
				}
			} else {
				for _, id := range e.InputIDs {
					if h.id == "msg_"+id {
						h.sources = []uint64{s.inputs[id].seq}
						break
					}
				}
			}
			l.trimEntry(h)
		}
		for _, id := range e.InputIDs {
			l.trimInput(id)
		}
	case protocol.InputPromoted:
		h := &s.history[len(s.history)-1]
		h.sources = append(h.sources, s.inputs[e.InputID].seq)
		l.trimEntry(h)
		l.trimInput(e.InputID)
	case protocol.ItemCompleted:
		h := &s.history[len(s.history)-1]
		h.sources = []uint64{env.Seq}
		l.trimEntry(h)
	case protocol.RequestOpened:
		l.asked[e.RequestID] = env.Seq
		delete(s.asked, e.RequestID)
	case protocol.RequestResolved:
		l.resolved[e.RequestID] = env.Seq
		delete(s.resolved, e.RequestID)
		if n := len(s.history); n > 0 && s.history[n-1].seq == env.Seq {
			h := &s.history[n-1]
			h.sources = []uint64{env.Seq}
			l.trimEntry(h)
		}
	case protocol.CommandRecorded:
		l.commands[e.InputID] = env.Seq
		c := s.commands[e.InputID]
		c.rec = protocol.CommandRecorded{InputID: e.InputID, Status: e.Status}
		s.commands[e.InputID] = c
	case protocol.ToolResultRetained:
		l.retained = append(l.retained, env.Seq)
		s.retained[len(s.retained)-1] = protocol.ToolResultRetained{Handle: e.Handle, BlobKey: e.BlobKey}
	}
}

func (l *LightState) trimInput(id string) {
	in := l.state.inputs[id]
	in.event = protocol.InputAdmitted{}
	l.state.inputs[id] = in
}

func (l *LightState) trimEntry(h *entry) {
	if len(h.msg.Parts) > 0 && h.msg.Parts[0].Type == protocol.LogPartToolResult && len(h.sources) == 1 {
		p := h.msg.Parts[0]
		h.msg.Parts = []protocol.LogPart{{Type: p.Type, CallID: p.CallID, Name: p.Name}}
	} else {
		h.msg.Parts = nil
	}
	if h.promoted != nil {
		h.promoted = []protocol.InputAdmitted{}
	}
}

// Head returns the last applied sequence.
func (l *LightState) Head() uint64 { return l.state.Head() }

// Model returns the current model.
func (l *LightState) Model() string { return l.state.Model() }

// Summary returns the session summary.
func (l *LightState) Summary() Summary { return l.state.Summary() }

// Settings returns the current settings.
func (l *LightState) Settings() protocol.Settings { return l.state.Settings() }

// Progress returns the current progress.
func (l *LightState) Progress() protocol.Progress { return l.state.Progress() }

// Agent returns the agent profile.
func (l *LightState) Agent() string { return l.state.Agent() }

// Usage returns the accumulated usage.
func (l *LightState) Usage() protocol.Usage { return l.state.Usage() }

// AllowedTools returns the allowed tools.
func (l *LightState) AllowedTools() []string { return l.state.AllowedTools() }

// MaxSteps returns the step limit.
func (l *LightState) MaxSteps() int { return l.state.MaxSteps() }

// Context returns the current context measurement.
func (l *LightState) Context() protocol.ContextMeasured { return l.state.Context() }

// CacheExpiry returns the cache expiry.
func (l *LightState) CacheExpiry() time.Time { return l.state.CacheExpiry() }

// CompactionCount returns the compaction count.
func (l *LightState) CompactionCount() int { return l.state.CompactionCount() }

// SubscriptionUsage returns the current subscription usage.
func (l *LightState) SubscriptionUsage() *protocol.SubscriptionUsage {
	return l.state.SubscriptionUsage()
}

// Resumable reports whether the session has pending work.
func (l *LightState) Resumable() bool { return l.state.Resumable() }

// QueuedInputs returns inputs waiting for a turn.
func (l *LightState) QueuedInputs() []protocol.QueuedInput { return l.state.QueuedInputs() }

// Attachment returns metadata for an input attachment.
func (l *LightState) Attachment(key string) (string, int, bool) {
	v, ok := l.attachments[key]
	return v.MediaType, v.Size, ok
}

// PagePlan describes a page and the source records needed to render it.
type PagePlan struct {
	entries            []lightEntry
	commands           []lightCommand
	first, last, total uint64
}
type lightCommand struct {
	seq, latest uint64
	after       string
}

// PageSources selects a page and returns the source sequences it needs.
func (l *LightState) PageSources(before uint64, limit int) (PagePlan, []uint64) {
	var all []lightEntry
	all = append(all, l.folded...)
	if l.state.compactedAt != 0 {
		all = append(all, lightEntry{Summary: l.state.compactedAt, At: l.state.compactedTime})
	}
	for _, h := range l.state.history {
		if !h.pinned && !h.carried {
			all = append(all, lightEntry{Entry: h})
		}
	}
	if limit <= 0 {
		limit = protocol.DefaultMessageLimit
	}
	total := uint64(len(all))
	hi := total
	if before > 0 && before-1 < hi {
		hi = before - 1
	}
	lo := uint64(0)
	if hi > uint64(limit) {
		lo = hi - uint64(limit)
	}
	p := PagePlan{entries: slices.Clone(all[lo:hi]), total: total}
	if hi > lo {
		p.first, p.last = lo+1, hi
	}
	ids := map[string]bool{}
	var seqs []uint64
	for _, x := range p.entries {
		if x.Summary != 0 {
			ids[summaryID(x.Summary)] = true
			seqs = append(seqs, x.Summary)
		} else {
			ids[x.Entry.id] = true
			seqs = append(seqs, x.Entry.sources...)
		}
	}
	for id, c := range l.state.commands {
		if c.after == "" && (p.first == 1 || total == 0) || ids[c.after] {
			p.commands = append(p.commands, lightCommand{c.seq, l.commands[id], c.after})
			seqs = append(seqs, l.commands[id])
		}
	}
	slices.SortFunc(p.commands, func(a, b lightCommand) int {
		if a.seq < b.seq {
			return -1
		}
		if a.seq > b.seq {
			return 1
		}
		return 0
	})
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	return p, seqs
}

// RenderPage renders a page from exactly the source records selected by PageSources.
func (l *LightState) RenderPage(p PagePlan, records []Record) (protocol.MessagePage, error) {
	var required []uint64
	for _, x := range p.entries {
		if x.Summary != 0 {
			required = append(required, x.Summary)
		} else {
			required = append(required, x.Entry.sources...)
		}
	}
	for _, c := range p.commands {
		required = append(required, c.latest)
	}
	slices.Sort(required)
	required = slices.Compact(required)
	if len(records) != len(required) {
		return protocol.MessagePage{}, fmt.Errorf("eventlog: wrong number of page sources")
	}
	events := make(map[uint64]Envelope, len(records))
	for _, r := range records {
		if _, exists := events[r.Seq]; exists || !slices.Contains(required, r.Seq) {
			return protocol.MessagePage{}, fmt.Errorf("eventlog: unexpected page source %d", r.Seq)
		}
		e, err := Decode(r.Data)
		if err != nil {
			return protocol.MessagePage{}, err
		}
		if e.Seq != r.Seq {
			return protocol.MessagePage{}, ErrSeq
		}
		events[r.Seq] = e
	}
	out := protocol.MessagePage{Messages: []protocol.Message{}, Commands: []protocol.MessageCommand{}, FirstSeq: p.first, LastSeq: p.last, Total: p.total, HasMore: p.first > 1}
	for _, x := range p.entries {
		m, err := renderLightEntry(x, events)
		if err != nil {
			return protocol.MessagePage{}, err
		}
		out.Messages = append(out.Messages, m)
	}
	for _, c := range p.commands {
		e, ok := events[c.latest]
		if !ok {
			return protocol.MessagePage{}, fmt.Errorf("eventlog: missing source %d", c.latest)
		}
		r, ok := e.Event.(protocol.CommandRecorded)
		if !ok {
			return protocol.MessagePage{}, fmt.Errorf("eventlog: invalid command source %d", c.latest)
		}
		out.Commands = append(out.Commands, protocol.MessageCommand{InputID: r.InputID, Line: r.Line, Name: r.Name, Args: r.Args, Status: r.Status, Text: r.Text, Result: r.Result, ResultTruncated: r.ResultTruncated, AfterMessageID: c.after, Seq: c.seq})
	}
	return out, nil
}

func renderLightEntry(x lightEntry, events map[uint64]Envelope) (protocol.Message, error) {
	if x.Summary != 0 {
		e, ok := events[x.Summary]
		if !ok {
			return protocol.Message{}, fmt.Errorf("eventlog: missing source %d", x.Summary)
		}
		c, ok := e.Event.(protocol.CompactionApplied)
		if !ok {
			return protocol.Message{}, fmt.Errorf("eventlog: invalid summary source %d", x.Summary)
		}
		return protocol.Message{ID: summaryID(x.Summary), Role: protocol.RoleUser, CreatedAt: x.At, Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: c.Summary}}}, nil
	}
	h := x.Entry
	var ins []protocol.InputAdmitted
	for _, seq := range h.sources {
		e, ok := events[seq]
		if !ok {
			return protocol.Message{}, fmt.Errorf("eventlog: missing source %d", seq)
		}
		switch v := e.Event.(type) {
		case protocol.InputAdmitted:
			ins = append(ins, v)
		case protocol.ItemCompleted:
			h.msg = v.Message
		case protocol.RequestResolved:
			if h.readerOnly {
				h.msg = protocol.LogMessage{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: v.Response}}}
			} else if len(h.msg.Parts) > 0 {
				part := h.msg.Parts[0]
				h.msg = dismissal(OpenToolCall{CallID: part.CallID, Name: part.Name}, v.SourceLabel)
			}
		default:
			return protocol.Message{}, fmt.Errorf("eventlog: invalid message source %d", seq)
		}
	}
	if len(ins) > 0 {
		if h.promoted != nil {
			h.msg = SteerMessage(partsOf(ins))
			h.promoted = ins
		} else {
			h.msg = protocol.LogMessage{Role: protocol.RoleUser, Parts: withoutTaskReports(ins[0].Parts)}
		}
	}
	return readerMessage(h), nil
}
