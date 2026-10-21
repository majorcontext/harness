package turn

import "slices"

// Pin is one message of engine context that the model reads after the first
// At messages of the history.
type Pin struct {
	Kind, Text string
	At         int
}

// Pins holds the pins of one session in the order the model reads them. A pin
// changes only when a compaction moves or retires it. The pins live in memory: a new
// process makes them again at its first model call, and the banner, which
// comes from the log, comes out the same. The session actor owns Pins.
type Pins struct {
	pins []Pin
}

// Set makes a pin of kind after the first at messages of the history when
// text differs from the newest pin of kind. An empty text makes a pin of
// cleared instead, once, after a pin that had text.
func (p *Pins) Set(kind, text, cleared string, at int) {
	var last string
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
// superseded text.
func (p *Pins) Move(n int) {
	kept := p.pins[:0]
	for i, q := range p.pins {
		if slices.ContainsFunc(p.pins[i+1:], func(r Pin) bool { return r.Kind == q.Kind }) {
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
