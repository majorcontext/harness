package harness

import (
	"fmt"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/protocol"
)

func TestOutcomeShowsTheTextOfEveryCodedError(t *testing.T) {
	spec := command.NewRegistry().All()[0]
	for _, c := range codes {
		err := fmt.Errorf("%w: detail", c.Err)
		status, text, _, _ := outcome("x", spec, nil, err)
		if status == protocol.CommandFailed && text == "/x failed: internal error" {
			t.Errorf("an error with code %s reads as an internal error: %s", c.Code, text)
		}
	}
}
