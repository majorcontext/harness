package protocol

import (
	"errors"
	"net/http"
)

// Code names the kind of an Error. It is the code member of an HTTP error body.
type Code string

// Error codes of the HTTP API.
const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeSessionNotFound     Code = "session_not_found"
	CodeSessionExists       Code = "session_exists"
	CodeSessionNotOwned     Code = "session_not_owned"
	CodeInputConflict       Code = "input_conflict"
	CodeTurnMismatch        Code = "turn_mismatch"
	CodeSessionBusy         Code = "session_busy"
	CodeRequestNotPending   Code = "request_not_pending"
	CodeModelUnavailable    Code = "model_unavailable"
	CodePayloadTooLarge     Code = "payload_too_large"
	CodeDraining            Code = "draining"
	CodeInternal            Code = "internal"
	CodeNotAGitRepo         Code = "not_a_git_repo"
	CodeNoBase              Code = "no_base"
	CodeTooManyChanges      Code = "too_many_changes"
	CodeProcessNotFound     Code = "process_not_found"
	CodeBlobNotFound        Code = "blob_not_found"
	CodeUnauthorized        Code = "unauthorized"
	CodeSessionUnreplayable Code = "session_unreplayable"
)

// CauseKey is the Details key that names the stop cause of a session_not_owned Error.
const CauseKey = "cause"

// ErrorBody is the body of every HTTP error response.
type ErrorBody struct {
	Error Error `json:"error"`
}

// Error is the error of the Go API and of the HTTP body: a code from the Code constants and a message.
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// Error returns Message.
func (e *Error) Error() string { return e.Message }

// Is reports whether target is an *Error with the code of e, so that an error decoded from a body
// matches the sentinel of its code. A target whose Details hold a CauseKey matches only an Error with
// the same cause.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t.Code != e.Code {
		return false
	}
	want, ok := t.Details[CauseKey]
	if !ok {
		return true
	}
	w, wok := want.(string)
	got, gok := e.Details[CauseKey].(string)
	return wok && gok && w == got
}

// CodeOf returns the Code of the first *Error in the chain of err, or CodeInternal.
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return CodeInternal
}

var statuses = map[Code]int{
	CodeInvalidRequest:      http.StatusBadRequest,
	CodeSessionNotFound:     http.StatusNotFound,
	CodeSessionExists:       http.StatusConflict,
	CodeSessionNotOwned:     http.StatusConflict,
	CodeInputConflict:       http.StatusConflict,
	CodeTurnMismatch:        http.StatusConflict,
	CodeSessionBusy:         http.StatusConflict,
	CodeRequestNotPending:   http.StatusConflict,
	CodeModelUnavailable:    http.StatusConflict,
	CodePayloadTooLarge:     http.StatusRequestEntityTooLarge,
	CodeDraining:            http.StatusServiceUnavailable,
	CodeNotAGitRepo:         http.StatusConflict,
	CodeNoBase:              http.StatusConflict,
	CodeTooManyChanges:      http.StatusConflict,
	CodeProcessNotFound:     http.StatusNotFound,
	CodeBlobNotFound:        http.StatusNotFound,
	CodeUnauthorized:        http.StatusUnauthorized,
	CodeInternal:            http.StatusInternalServerError,
	CodeSessionUnreplayable: http.StatusInternalServerError,
}

// HTTPStatus returns the status of the Errors table for code, or 500 for an unknown code.
// The Sync receiver codes are not in the table.
func HTTPStatus(code Code) int {
	if s, ok := statuses[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}
