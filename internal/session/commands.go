package session

import (
	"context"
	"strings"
	"unicode"

	"github.com/majorcontext/harness/internal/eventlog"
)

const commandAccepted = "accepted"

// Record appends c, the status of the command of input c.InputID. A record
// that repeats the line of the command, but cannot follow its newest
// status, appends nothing and returns that status with repeat true. While
// a run is on, a non-nil busy replaces c. Record returns the newest record
// and the seq of the first record of the command.
func (a *Actor) Record(ctx context.Context, c eventlog.CommandRecorded, busy *eventlog.CommandRecorded) (eventlog.CommandRecorded, uint64, bool, error) {
	type recorded struct {
		rec    eventlog.CommandRecorded
		seq    uint64
		repeat bool
	}
	r, err := call(ctx, a, func(reply func(recorded, error)) {
		if _, _, ok := a.state.Input(c.InputID); ok {
			reply(recorded{}, ErrInputConflict)
			return
		}
		if prior, seq, ok := a.state.Command(c.InputID); ok {
			switch {
			case prior.Line != c.Line:
				reply(recorded{}, ErrInputConflict)
				return
			case c.Status == commandAccepted || prior.Status != commandAccepted:
				reply(recorded{prior, seq, true}, nil)
				return
			}
		}
		if busy != nil && a.run != nil {
			c = *busy
		}
		err := a.append(c)
		rec, seq, _ := a.state.Command(c.InputID)
		reply(recorded{rec, seq, false}, err)
	})
	return r.rec, r.seq, r.repeat, err
}

// endCommands records each command that an earlier owner accepted and
// never finished as interrupted. It never runs one again.
func (a *Actor) endCommands(ctx context.Context) error {
	var events []eventlog.Event
	for _, c := range a.state.Unfinished() {
		c.Status, c.Text, c.Result, c.ResultTruncated = "interrupted", "harness restarted before /"+Typed(c.Line)+" finished; it will not run again", nil, false
		events = append(events, c)
	}
	if len(events) == 0 {
		return nil
	}
	return a.appendCtx(ctx, events...)
}

// Typed returns the command name of line as the user typed it.
func Typed(line string) string {
	body := strings.TrimPrefix(line, "/")
	if i := strings.IndexFunc(body, unicode.IsSpace); i >= 0 {
		return body[:i]
	}
	return body
}
