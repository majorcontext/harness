package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
)

const (
	// reportInline bounds the result that a report keeps whole, and the
	// preview of a larger one, in bytes.
	reportInline = 4096
	// reasonCap and hintCap bound the cause and the recover hint of a failed
	// child in a report, in runes.
	reasonCap = 500
	hintCap   = 120
	// reportTool names the tool of a retained result of a report.
	reportTool = "task"
	// cutUnread ends the preview of a result that no handle backs.
	cutUnread = "… [truncated; full result unavailable]"
)

// boundedText masks s, then cuts it to n runes: the one rule for text of a
// provider that a report to a parent carries.
func boundedText(s string, n int) string {
	s, _ = CapRunes(toolresult.Mask(s), n)
	return s
}

// bigResult is a result that is longer than reportInline: its masked text, and
// the key of the blob that holds it when a handle can back the preview.
type bigResult struct{ masked, key string }

// stage masks the result of a done child that is longer than reportInline,
// and writes it to a blob, before the record that names it, when the parent
// can read the rest back and the child is still unsettled. It returns no key
// when the parent cannot, and no text for a result that stays whole as it is.
func (a *Actor) stage(ctx context.Context, id string, rep *Report) bigResult {
	if rep.outcome != eventlog.OutcomeDone || !a.cfg.Retain || len(rep.result) <= reportInline {
		return bigResult{}
	}
	big := bigResult{masked: toolresult.Mask(rep.result)}
	if len(big.masked) <= reportInline {
		return big
	}
	key, err := call(ctx, a, func(reply func(string, error)) {
		if !slices.Contains(a.state.Unsettled(), id) || !a.canRetain(len(big.masked)) {
			reply("", nil)
			return
		}
		reply(fmt.Sprintf("report-%s-%d", rep.turn, a.fenced), nil)
	})
	if err != nil || key == "" || a.cfg.Store.PutBlob(a.cfg.Base, key, strings.NewReader(big.masked)) != nil {
		return big
	}
	big.key = key
	return big
}

// shown returns the result of a report as the parent reads it, and the record
// of the result that it retains. Without retention a result is cut at
// ResultCap runes. With it, a result of at most reportInline bytes stays
// whole, and a larger one shows a preview of the masked text that names the
// handle of the rest, or says that the rest is unavailable.
func (a *Actor) shown(rep *Report, big bigResult) (string, []eventlog.Event) {
	switch {
	case rep.outcome != eventlog.OutcomeDone || !a.cfg.Retain:
		text, _ := CapRunes(rep.result, ResultCap)
		return text, nil
	case big.masked == "":
		return rep.result, nil
	case len(big.masked) <= reportInline:
		return big.masked, nil
	}
	preview := toolresult.Cut(big.masked, reportInline)
	if big.key == "" || !a.canRetain(len(big.masked)) {
		return preview + cutUnread, nil
	}
	_, last := a.retainedTotals()
	m := toolresult.NewMeta(toolresult.Handle(last+1), reportTool, big.key, big.masked)
	return preview + fmt.Sprintf(" … [full result retained — read the rest with read_tool_result(handle=%q)]", m.Handle),
		[]eventlog.Event{eventlog.ToolResultRetained{Handle: m.Handle, Tool: m.Tool, BlobKey: m.Key, Bytes: m.Bytes, Lines: m.Lines, Head: m.Head}}
}

// canRetain reports whether the parent can read back a retained result of n
// bytes: it has the read_tool_result tool on a backend that runs it, and the
// retained total stays inside the budget.
func (a *Actor) canRetain(n int) bool {
	if !a.cfg.Retain || a.ownsLoop() || len(turn.Restrict([]turn.Tool{toolresult.NewTool(a)}, a.state.AllowedTools())) == 0 {
		return false
	}
	used, _ := a.retainedTotals()
	return used+n <= toolresult.Budget
}

// retainedTotals returns the bytes of the retained results, and the number of
// the newest handle.
func (a *Actor) retainedTotals() (used, last int) {
	for _, r := range a.state.Retained() {
		used += r.Bytes
		last = max(last, handleNumber(r.Handle))
	}
	return used, last
}

func handleNumber(h string) int {
	n, _ := toolresult.Number(h)
	return n
}
