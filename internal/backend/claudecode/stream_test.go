package claudecode

import (
	"errors"
	"testing"
)

func TestStopErrorLetsTheStopWin(t *testing.T) {
	cause := errors.New("stopped")
	other := errors.New("other")
	success := &envelope{}
	failed := &envelope{IsError: true}
	for _, tc := range []struct {
		name string
		err  error
		res  *envelope
		want error
	}{
		{"exit with no result", errExited, nil, cause},
		{"cause with no result", cause, nil, cause},
		{"no error with a success result", nil, success, nil},
		{"no error with an error result", nil, failed, cause},
		{"other error", other, success, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopError(tc.err, cause, tc.res); got != tc.want {
				t.Errorf("stopError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
