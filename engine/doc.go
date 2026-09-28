// Package engine runs agent sessions.
//
// A [Session] sends its history to a model provider, runs the tools that the
// model calls, and records each step in an append-only log. The engine is
// headless: the harness CLI and HTTP server are clients of this package.
//
// Create a session with [NewSession] and send it a prompt with
// [Session.Prompt]. [Config] selects the providers, the model, the working
// directory for built-in tools, and any extra [Tool] values. Set
// [Config.OnEvent] to receive text deltas, tool calls, and results as they
// occur. Change the model at any time with [Session.SetModel]; history stays
// in a provider-neutral form, so no migration is necessary.
//
// Set [Config.SessionDir] to persist sessions. [LoadSession] resumes a
// persisted session by its ID.
package engine
