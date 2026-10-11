package protocol

// SyncBatch is a remote append of session records from FromSeq, under the sender's Ownership epoch.
type SyncBatch struct {
	Epoch   uint64            `json:"epoch"`
	Session string            `json:"session"`
	FromSeq uint64            `json:"from_seq"`
	Records [][]byte          `json:"records"`
	Blobs   map[string][]byte `json:"blobs,omitempty"`
}

// SyncAck is the head of the receiver after a SyncBatch, also on a seq mismatch.
type SyncAck struct {
	Head uint64 `json:"head"`
}

// Sync wire contract. A sender stops a session on 409 with CodeStaleEpoch or
// CodeSyncConflict, 400, any 413, 401, or 403, and resends for any other answer.
const (
	MaxSyncBatchBytes = 32 << 20 // largest JSON body of a SyncBatch; a receiver answers 413 with CodePayloadTooLarge above it
	CodeStaleEpoch    = "stale_epoch"
	CodeSyncConflict  = "sync_conflict"
)
