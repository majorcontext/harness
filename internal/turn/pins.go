package turn

import (
	"slices"
	"sync"
)

// Pin is one message of engine context that the model reads after the first
// At messages of the history.
type Pin struct {
	Kind, Text string
	At         int
}

// Pins holds the pins of one session in the order the model reads them. A pin
// never changes once made, so each request is a prefix of the next. The pins
// live in memory: a restart makes them again from the live state.
type Pins struct {
	mu   sync.Mutex
	pins []Pin
}

// Set makes a pin of kind after the first at messages of the history when
// text differs from the newest pin of kind. An empty text makes a pin of
// cleared instead, once, after a pin that had text. A nil Pins ignores Set.
func (p *Pins) Set(kind, text, cleared string, at int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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

// Clamp moves each pin that is past a history of n messages to its end.
func (p *Pins) Clamp(n int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.pins {
		p.pins[i].At = min(p.pins[i].At, n)
	}
}

// Pinned returns the pins in the order the model reads them.
func (p *Pins) Pinned() []Pin {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.pins)
}
