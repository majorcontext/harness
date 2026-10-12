package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/majorcontext/harness/protocol"
)

var (
	// ErrUnknownKind reports an event kind with no registered type.
	ErrUnknownKind = protocol.ErrUnknownKind
	// ErrVersion reports an envelope version that this package cannot read.
	ErrVersion = errors.New("eventlog: unsupported envelope version")
	// ErrSeq reports a record whose seq does not follow the head.
	ErrSeq = protocol.ErrSeq
)

// Record is one stored record: its seq and its opaque bytes.
type Record struct {
	Seq  uint64
	Data []byte
}

// Envelope is one decoded record. Decode holds the payload as a value, such as
// protocol.TurnEnded.
type Envelope struct {
	Seq   uint64
	Time  time.Time
	Event protocol.Payload
}

type wireEnvelope struct {
	V   int             `json:"v"`
	Seq uint64          `json:"seq"`
	T   time.Time       `json:"t"`
	K   string          `json:"k"`
	D   json.RawMessage `json:"d"`
}

// Encode returns the record bytes of e.
func (e Envelope) Encode() ([]byte, error) {
	if e.Event == nil {
		return nil, fmt.Errorf("%w: nil event", ErrUnknownKind)
	}
	k := e.Event.Kind()
	if !protocol.IsDurableKind(k) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, k)
	}
	d, err := json.Marshal(e.Event)
	if err != nil {
		return nil, fmt.Errorf("eventlog: encode %s: %w", k, err)
	}
	return json.Marshal(wireEnvelope{V: protocol.EnvelopeVersion, Seq: e.Seq, T: e.Time.UTC(), K: k, D: d})
}

// Decode parses record bytes. An unknown kind or version fails.
func Decode(data []byte) (Envelope, error) {
	var w wireEnvelope
	if err := json.Unmarshal(data, &w); err != nil {
		return Envelope{}, fmt.Errorf("eventlog: decode envelope: %w", err)
	}
	if w.V != protocol.EnvelopeVersion {
		return Envelope{}, fmt.Errorf("%w: %d", ErrVersion, w.V)
	}
	ev, err := payloadValue(protocol.Event{Kind: w.K, Data: w.D})
	if err != nil {
		return Envelope{}, fmt.Errorf("eventlog: %w", err)
	}
	return Envelope{Seq: w.Seq, Time: w.T, Event: ev}, nil
}

// payloadValue decodes e with Event.Payload and returns the payload by value.
func payloadValue(e protocol.Event) (protocol.Payload, error) {
	p, err := e.Payload()
	if err != nil {
		return nil, err
	}
	return reflect.ValueOf(p).Elem().Interface().(protocol.Payload), nil
}
