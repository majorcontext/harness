// Package message defines the canonical types of session history.
//
// A [Message] holds [Parts]: text, reasoning, binary blobs, tool calls, and
// tool results. History stores these types and never provider wire objects.
// Each provider adapter transcodes them for its own API on every request.
//
// A [Message] records the [modelref.ModelRef] of the model that produced it.
package message
