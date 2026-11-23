package turn

import "slices"

// Pin is one message of engine context that the model reads after the first
// At messages of the history.
type Pin struct {
	Kind, Text string
	At         int
	// Old is true from a declaration until the next call pins, so that only the call of the declaration retires a pin.
	Old bool
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

// MarkOld marks each pin as older than the declaration that was just made.
func (p *Pins) MarkOld() { p.setOld(true) }

// ClearOld ends the check of the pins against the newest declaration.
func (p *Pins) ClearOld() { p.setOld(false) }

func (p *Pins) setOld(old bool) {
	p.pins = slices.Clone(p.pins)
	for i := range p.pins {
		p.pins[i].Old = old
	}
}

// Retire drops every pin of kind when the newest one is older than the
// newest declaration and its text is not text, because the declaration states
// what the pin said. It reports whether it dropped any.
func (p *Pins) Retire(kind, text string) bool {
	var newest *Pin
	for i := range slices.Backward(p.pins) {
		if p.pins[i].Kind == kind {
			newest = &p.pins[i]
			break
		}
	}
	if newest == nil || !newest.Old || newest.Text == text {
		return false
	}
	p.pins = slices.DeleteFunc(slices.Clone(p.pins), func(q Pin) bool { return q.Kind == kind })
	return true
}

// Has reports whether a pin of kind exists.
func (p *Pins) Has(kind string) bool {
	return slices.ContainsFunc(p.pins, func(q Pin) bool { return q.Kind == kind })
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
