package server

import "errors"

// ErrSessionDirLocked reports that another process already holds the
// session directory. One journal must have one writer: two servers on one
// directory interleave two seq streams into a single events.jsonl.
var ErrSessionDirLocked = errors.New("another harness process owns this session directory")
