package turn

import "slices"

// Pin is one message of engine context that the model reads after the first
// At messages of the history.
type Pin struct {
	Kind, Text string
	At         int
}

// Pins holds the pins of one session in the order the model reads them. A pin
// changes only when a compaction moves it, so each request is a prefix of the
// next until a compaction applies. The pins live in memory: a restart makes
// them again from the live state. Pins has no lock: the session actor owns it
// and a turn reaches it through Turn.Pin.
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

// Move puts every pin after the first n messages of the history.
func (p *Pins) Move(n int) {
	for i := range p.pins {
		p.pins[i].At = n
	}
}

// Pinned returns a copy of the pins in the order the model reads them.
func (p *Pins) Pinned() []Pin {
	return slices.Clone(p.pins)
}
