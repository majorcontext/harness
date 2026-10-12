package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownKind reports an event kind with no durable payload type.
var ErrUnknownKind = errors.New("protocol: unknown event kind")

var payloads = map[string]func() Payload{
	KindSessionCreated:     func() Payload { return new(SessionCreated) },
	KindOwnerAcquired:      func() Payload { return new(OwnerAcquired) },
	KindSettingsChanged:    func() Payload { return new(SettingsChanged) },
	KindInputAdmitted:      func() Payload { return new(InputAdmitted) },
	KindInputPromoted:      func() Payload { return new(InputPromoted) },
	KindInputWithdrawn:     func() Payload { return new(InputWithdrawn) },
	KindTurnStarted:        func() Payload { return new(TurnStarted) },
	KindItemCompleted:      func() Payload { return new(ItemCompleted) },
	KindTurnSuspended:      func() Payload { return new(TurnSuspended) },
	KindTurnResumed:        func() Payload { return new(TurnResumed) },
	KindTurnEnded:          func() Payload { return new(TurnEnded) },
	KindRequestOpened:      func() Payload { return new(RequestOpened) },
	KindRequestResolved:    func() Payload { return new(RequestResolved) },
	KindGoalSet:            func() Payload { return new(GoalSet) },
	KindGoalEvaluated:      func() Payload { return new(GoalEvaluated) },
	KindGoalChanged:        func() Payload { return new(GoalChanged) },
	KindCompactionApplied:  func() Payload { return new(CompactionApplied) },
	KindChildSpawned:       func() Payload { return new(ChildSpawned) },
	KindChildSettled:       func() Payload { return new(ChildSettled) },
	KindCommandRecorded:    func() Payload { return new(CommandRecorded) },
	KindContextMeasured:    func() Payload { return new(ContextMeasured) },
	KindBackendState:       func() Payload { return new(BackendState) },
	KindToolResultRetained: func() Payload { return new(ToolResultRetained) },
}

// IsDurableKind reports whether kind is the kind of a durable event.
func IsDurableKind(kind string) bool {
	_, ok := payloads[kind]
	return ok
}

// Payload decodes e.Data into a pointer to the payload type of e.Kind, such as
// *TurnEnded. An ephemeral frame or an unknown kind fails with ErrUnknownKind.
func (e Event) Payload() (Payload, error) {
	mk, ok := payloads[e.Kind]
	if !ok || e.Ephemeral {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, e.Kind)
	}
	p := mk()
	if err := json.Unmarshal(e.Data, p); err != nil {
		return nil, fmt.Errorf("protocol: decode %s: %w", e.Kind, err)
	}
	return p, nil
}
