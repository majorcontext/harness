package protocol

// Progress is the status that a log folds to at Seq, as plain JSON that a reader stores with a cursor.
// Queued and Requests hold input and request IDs, oldest first. TurnID is empty while a turn is suspended.
// Status is empty until the first event. Goal and LastTurn are nil until a goal is set or a turn ends. The zero Progress is an empty log.
type Progress struct {
	Seq      uint64    `json:"seq"`
	Status   Status    `json:"status,omitempty"`
	TurnID   string    `json:"turn_id,omitempty"`
	Queued   []string  `json:"queued,omitempty"`
	Requests []string  `json:"requests,omitempty"`
	Goal     *GoalView `json:"goal,omitempty"`
	LastTurn *LastTurn `json:"last_turn,omitempty"`
}
