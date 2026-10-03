// Package eventlog defines the session event schema, the record codec, and
// Apply, the one function that builds session State from the log.
//
// A session log is append-only. Each record is one Envelope with a gap-free
// seq. Live code appends a record and then applies the same record; replay
// applies the stored records in order. Check runs the same transitions on a
// copy of State, so the session actor can reject an illegal batch before it
// appends it.
package eventlog
