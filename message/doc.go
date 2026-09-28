// Package message defines the canonical types of session history.
//
// A [Message] holds [Parts]: text, reasoning, binary blobs, tool calls, and
// tool results. History stores these types and never provider wire objects.
// Each provider adapter transcodes them for its own API on every request.
//
// A [ModelRef] names a model as "provider/model". Use [ParseModelRef] to
// read one from a string.
package message
