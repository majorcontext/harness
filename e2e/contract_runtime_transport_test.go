package e2e

import (
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

type declaredError struct {
	msg       string
	permanent bool
}

func (e declaredError) Error() string   { return e.msg }
func (e declaredError) Permanent() bool { return e.permanent }

type failingTransport struct {
	err      error
	attempts atomic.Int64
	// arrived, when set, receives once per request; the request then waits for
	// its context to end before it fails with err.
	arrived chan struct{}
}

func (f *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.attempts.Add(1)
	if f.arrived != nil {
		f.arrived <- struct{}{}
		<-req.Context().Done()
	}
	return nil, f.err
}

func TestContractRuntimeTransport(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TRANSPORT_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a_transport_error_that_declares_itself_permanent_makes_one_attempt_and_ends_the_turn_permanent", rowAPermanentTransportErrorMakesOneAttempt},
		{"a_permanent_transport_error_inside_a_wrapping_error_makes_one_attempt", rowAPermanentTransportErrorInsideAWrappingErrorMakesOneAttempt},
		{"an_ordinary_transport_error_keeps_both_retry_layers", rowAnOrdinaryTransportErrorKeepsBothRetryLayers},
		{"a_canceled_request_with_a_permanent_transport_error_stays_an_abort", rowACanceledRequestWithAPermanentTransportErrorStaysAnAbort},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.run(t)
		})
	}
}

func transportTurn(t *testing.T, err error) (harness.TurnResult, *failingTransport) {
	t.Helper()
	ft := &failingTransport{err: err}
	return waitTurn(t, promptTurn(t, transportSession(t, ft), textInput("a", "go"))), ft
}

func transportSession(t *testing.T, ft *failingTransport) *harness.Session {
	t.Helper()
	r, nerr := harness.New(harness.Options{
		Store:          store.NewMemStore(),
		Getenv:         os.Getenv,
		ModelTransport: func(string) http.RoundTripper { return ft },
		Config: config.Config{
			ContextWindowTokens: 100000,
			RequestMaxRetries:   new(1),
			StreamMaxRetries:    new(1),
			Providers:           map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://model.invalid", APIKeyEnv: "HARNESS_E2E_TRANSPORT_KEY"}},
		},
	})
	if nerr != nil {
		t.Fatal(nerr)
	}
	t.Cleanup(func() { _ = r.Close(t.Context()) })
	s, cerr := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if cerr != nil {
		t.Fatal(cerr)
	}
	return s
}

func rowAPermanentTransportErrorMakesOneAttempt(t *testing.T) {
	res, ft := transportTurn(t, declaredError{msg: "fetch failed", permanent: true})
	if n := ft.attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
	if res.StopReason != protocol.StopFailed || res.ErrorClass != protocol.ErrorPermanent {
		t.Errorf("result = %+v, want a failed turn of class permanent", res)
	}
}

func rowAPermanentTransportErrorInsideAWrappingErrorMakesOneAttempt(t *testing.T) {
	res, ft := transportTurn(t, fmt.Errorf("dial layer: %w", declaredError{msg: "fetch failed", permanent: true}))
	if n := ft.attempts.Load(); n != 1 || res.ErrorClass != protocol.ErrorPermanent {
		t.Errorf("attempts = %d, class = %q, want 1 and permanent", n, res.ErrorClass)
	}
}

func rowAnOrdinaryTransportErrorKeepsBothRetryLayers(t *testing.T) {
	res, ft := transportTurn(t, declaredError{msg: "connection reset"})
	if n := ft.attempts.Load(); n != 4 {
		t.Errorf("attempts = %d, want 4 (2 requests in each of 2 model calls)", n)
	}
	if res.StopReason != protocol.StopFailed || res.ErrorClass == protocol.ErrorPermanent {
		t.Errorf("result = %+v, want a failed turn of a retryable class", res)
	}
}

func rowACanceledRequestWithAPermanentTransportErrorStaysAnAbort(t *testing.T) {
	ft := &failingTransport{err: declaredError{msg: "fetch aborted", permanent: true}, arrived: make(chan struct{}, 4)}
	turn := promptTurn(t, transportSession(t, ft), textInput("a", "go"))
	<-ft.arrived
	if err := turn.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	res := waitTurn(t, turn)
	if n := ft.attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
	if res.StopReason != protocol.StopInterrupted || res.ErrorClass == protocol.ErrorPermanent {
		t.Errorf("result = %+v, want an interrupted turn that is not permanent", res)
	}
}
