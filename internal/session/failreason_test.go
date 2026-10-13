package session

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestFailReasonOfALoginFailureNamesTheLogin(t *testing.T) {
	got := failReason(protocol.TurnEnded{StopReason: protocol.StopFailed, Error: "not logged in", ErrorClass: protocol.ErrorLoginRequired})
	if !strings.HasPrefix(got, reasonLoginRequired+": ") || strings.Contains(got, "permanent") {
		t.Errorf("reason = %q, want the login prefix and the error text", got)
	}
}
