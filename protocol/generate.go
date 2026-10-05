package protocol

// A field tagged optional:"true" is not required on decode although its json
// tag has no omitempty, and the generator leaves it out of "required".

//go:generate go run ./internal/gen
