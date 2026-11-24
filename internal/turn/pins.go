package turn

import (
	"slices"

	"github.com/majorcontext/harness/protocol"
)

// Pin is one message of engine context that the model reads after the first
// At messages of the history.
type Pin struct {
	Kind, Text string
	At         int
	// Turn is the turn that recorded the pin. Empty: none known.
	Turn string
}

// Pins holds the pins of one session in the order the model reads them. A pin
// changes only when a compaction moves or retires it. The log records each pin
// but the banner. The session actor owns Pins.
type Pins struct {
	pins []Pin
}

// Add appends q with no check, for a replay of recorded pins.
func (p *Pins) Add(q Pin) { p.pins = append(p.pins, q) }

// Prepend puts q before every pin.
func (p *Pins) Prepend(q Pin) { p.pins = slices.Insert(p.pins, 0, q) }

// Retire drops every pin of kind when old is true and the newest pin of kind
// has another text than text, because a declaration states what the pin said.
// It reports whether it dropped any.
func (p *Pins) Retire(kind, text string, old bool) bool {
	var newest *Pin
	for i := range slices.Backward(p.pins) {
		if p.pins[i].Kind == kind {
			newest = &p.pins[i]
			break
		}
	}
	if newest == nil || !old || newest.Text == text {
		return false
	}
	p.pins = slices.DeleteFunc(slices.Clone(p.pins), func(q Pin) bool { return q.Kind == kind })
	return true
}

// Mark sets the turn of each pin from the first n pins on.
func (p *Pins) Mark(n int, turnID string) {
	p.pins = slices.Clone(p.pins)
	for i := n; i < len(p.pins); i++ {
		p.pins[i].Turn = turnID
	}
}

// Drop removes every pin of kind.
func (p *Pins) Drop(kind string) {
	p.pins = slices.DeleteFunc(slices.Clone(p.pins), func(q Pin) bool { return q.Kind == kind })
}

// Has reports whether a pin of kind exists.
func (p *Pins) Has(kind string) bool {
	return slices.ContainsFunc(p.pins, func(q Pin) bool { return q.Kind == kind })
}

// Set makes a pin of kind after the first at messages of the history when
// text differs from the newest pin of kind, or from initial when no pin of
// kind exists. An empty text makes a pin of cleared instead, once, after a pin
// that had text.
func (p *Pins) Set(kind, text, cleared, initial string, at int) {
	last := initial
	for _, q := range slices.Backward(p.pins) {
		if q.Kind == kind {
			last = q.Text
			break
		}
	}
	if text == "" {
		if last == "" || cleared == "" {
			return
		}
		text = cleared
	}
	if text == last {
		return
	}
	if n := len(p.pins); n > 0 {
		at = max(at, p.pins[n-1].At)
	}
	p.pins = append(p.pins, Pin{Kind: kind, Text: text, At: at})
}

// Move puts the newest pin of each kind after the first n messages of the
// history and drops the older pins of that kind, so a compaction retires each
// superseded text. It drops each code-mode pin: the declaration that follows a
// compaction states the mode.
func (p *Pins) Move(n int) {
	kept := p.pins[:0]
	for i, q := range p.pins {
		if q.Kind == protocol.PinCodeMode || slices.ContainsFunc(p.pins[i+1:], func(r Pin) bool { return r.Kind == q.Kind }) {
			continue
		}
		q.At = n
		kept = append(kept, q)
	}
	p.pins = kept
}

// Pinned returns a copy of the pins in the order the model reads them.
func (p *Pins) Pinned() []Pin {
	return slices.Clone(p.pins)
}
