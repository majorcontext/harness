package protocol_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestReasonDropsTheInvalidRequestSentinels(t *testing.T) {
	invalid := &protocol.Error{Code: protocol.CodeInvalidRequest, Message: "harness: invalid request"}
	plain := errors.New("invalid request")
	rejected := &protocol.Error{Code: protocol.CodeSessionBusy, Message: "harness: sync rejected"}
	for _, tc := range []struct {
		name  string
		err   error
		extra []error
		want  string
	}{
		{"wrapped sentinel", fmt.Errorf("%w: keep_turns must be >= 1", invalid), nil, "keep_turns must be >= 1"},
		{"repeated sentinel", fmt.Errorf("%w: %w", invalid, fmt.Errorf("%w: bad", invalid)), nil, "bad"},
		{"several wrapped errors", fmt.Errorf("%w: %w: seq 3", invalid, errors.New("why")), nil, "why: seq 3"},
		{"listed plain sentinel", fmt.Errorf("%w: scope bad", plain), []error{plain}, "scope bad"},
		{"unlisted plain sentinel", fmt.Errorf("%w: scope bad", plain), nil, "invalid request: scope bad"},
		{"other category keeps its text", fmt.Errorf("%w: %w: seq 3", invalid, rejected), nil, "harness: sync rejected: seq 3"},
		{"bare sentinel", invalid, nil, "harness: invalid request"},
		{"empty reason", fmt.Errorf("%w: ", invalid), nil, "harness: invalid request: "},
		{"sentinel text inside the reason", fmt.Errorf("%w: x harness: invalid request: y", invalid), nil, "x harness: invalid request: y"},
	} {
		if got := protocol.Reason(tc.err, tc.extra...); got != tc.want {
			t.Errorf("%s: Reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}
