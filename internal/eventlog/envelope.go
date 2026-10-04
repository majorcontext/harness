package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the envelope version that this package writes and reads.
const Version = 1

var (
	// ErrUnknownKind reports an event kind with no registered type.
	ErrUnknownKind = errors.New("eventlog: unknown event kind")
	// ErrVersion reports an envelope version that this package cannot read.
	ErrVersion = errors.New("eventlog: unsupported envelope version")
	// ErrSeq reports a record whose seq does not follow the head.
	ErrSeq = errors.New("eventlog: seq out of order")
)

// Record is one stored record: its seq and its opaque bytes.
type Record struct {
	Seq  uint64
	Data []byte
}

// Envelope is one decoded record.
type Envelope struct {
	Seq   uint64
	Time  time.Time
	Event Event
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
	if _, ok := registry[k]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, k)
	}
	d, err := json.Marshal(e.Event)
	if err != nil {
		return nil, fmt.Errorf("eventlog: encode %s: %w", k, err)
	}
	return json.Marshal(wireEnvelope{V: Version, Seq: e.Seq, T: e.Time.UTC(), K: k, D: d})
}

// Decode parses record bytes. An unknown kind or version fails.
func Decode(data []byte) (Envelope, error) {
	var w wireEnvelope
	if err := json.Unmarshal(data, &w); err != nil {
		return Envelope{}, fmt.Errorf("eventlog: decode envelope: %w", err)
	}
	if w.V != Version {
		return Envelope{}, fmt.Errorf("%w: %d", ErrVersion, w.V)
	}
	decode, ok := registry[w.K]
	if !ok {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownKind, w.K)
	}
	ev, err := decode(w.D)
	if err != nil {
		return Envelope{}, fmt.Errorf("eventlog: decode %s: %w", w.K, err)
	}
	return Envelope{Seq: w.Seq, Time: w.T, Event: ev}, nil
}

func decodeAs[E Event](d json.RawMessage) (Event, error) {
	var e E
	err := json.Unmarshal(d, &e)
	return e, err
}

var registry = map[string]func(json.RawMessage) (Event, error){
	SessionCreated{}.Kind():     decodeAs[SessionCreated],
	OwnerAcquired{}.Kind():      decodeAs[OwnerAcquired],
	SettingsChanged{}.Kind():    decodeAs[SettingsChanged],
	InputAdmitted{}.Kind():      decodeAs[InputAdmitted],
	InputPromoted{}.Kind():      decodeAs[InputPromoted],
	InputWithdrawn{}.Kind():     decodeAs[InputWithdrawn],
	TurnStarted{}.Kind():        decodeAs[TurnStarted],
	ItemCompleted{}.Kind():      decodeAs[ItemCompleted],
	TurnSuspended{}.Kind():      decodeAs[TurnSuspended],
	TurnResumed{}.Kind():        decodeAs[TurnResumed],
	TurnEnded{}.Kind():          decodeAs[TurnEnded],
	RequestOpened{}.Kind():      decodeAs[RequestOpened],
	RequestResolved{}.Kind():    decodeAs[RequestResolved],
	GoalSet{}.Kind():            decodeAs[GoalSet],
	GoalEvaluated{}.Kind():      decodeAs[GoalEvaluated],
	GoalChanged{}.Kind():        decodeAs[GoalChanged],
	CompactionApplied{}.Kind():  decodeAs[CompactionApplied],
	ChildSpawned{}.Kind():       decodeAs[ChildSpawned],
	ChildSettled{}.Kind():       decodeAs[ChildSettled],
	ContextMeasured{}.Kind():    decodeAs[ContextMeasured],
	BackendState{}.Kind():       decodeAs[BackendState],
	ToolResultRetained{}.Kind(): decodeAs[ToolResultRetained],
}
