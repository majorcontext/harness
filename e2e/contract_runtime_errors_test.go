package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/httpapi"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestContractRuntimeErrors(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"an_error_body_decodes_to_an_error_that_matches_the_sentinel_of_its_code", rowAnErrorBodyDecodesToAnErrorThatMatchesTheSentinelOfItsCode},
		{"a_stop_cause_reaches_a_client_in_the_error_frame_of_the_event_stream", rowAStopCauseReachesAClientInTheErrorFrameOfTheEventStream},
		{"every_sentinel_has_the_status_of_its_code", rowEverySentinelHasTheStatusOfItsCode},
		{"a_log_that_does_not_replay_fails_with_session_unreplayable", rowALogThatDoesNotReplayFailsWithSessionUnreplayable},
		{"new_refuses_a_bad_option_with_a_plain_error", rowNewRefusesABadOptionWithAPlainError},
		{"a_transport_error_that_declares_itself_permanent_makes_one_attempt_and_ends_the_turn_permanent", rowAPermanentTransportErrorMakesOneAttempt},
		{"a_permanent_transport_error_inside_a_wrapping_error_makes_one_attempt", rowAPermanentTransportErrorInsideAWrappingErrorMakesOneAttempt},
		{"an_ordinary_transport_error_keeps_both_retry_layers", rowAnOrdinaryTransportErrorKeepsBothRetryLayers},
		{"a_canceled_request_with_a_permanent_transport_error_stays_an_abort", rowACanceledRequestWithAPermanentTransportErrorStaysAnAbort},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func errorBodyOf(t *testing.T, h http.Handler, method, path string) (int, *protocol.Error) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body protocol.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: body %q: %v", method, path, rec.Body.String(), err)
	}
	return rec.Code, &body.Error
}

func rowAnErrorBodyDecodesToAnErrorThatMatchesTheSentinelOfItsCode(t *testing.T) {
	r := lifetimeRuntimeOn(t, store.NewMemStore(), nil)
	status, remote := errorBodyOf(t, httpapi.Handler(r), http.MethodGet, "/sessions/nope")
	if status != http.StatusNotFound || !errors.Is(remote, harness.ErrSessionNotFound) {
		t.Errorf("status = %d, error = %v; want 404 and a match for ErrSessionNotFound", status, remote)
	}
	if errors.Is(remote, harness.ErrSessionExists) || errors.Is(remote, harness.ErrSessionNotOwned) {
		t.Errorf("%v matches a sentinel of another code", remote)
	}
	if got := protocol.CodeOf(remote); got != protocol.CodeSessionNotFound {
		t.Errorf("CodeOf = %q, want %q", got, protocol.CodeSessionNotFound)
	}
}

func rowEverySentinelHasTheStatusOfItsCode(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   protocol.Code
		status int
	}{
		{harness.ErrInvalidRequest, protocol.CodeInvalidRequest, http.StatusBadRequest},
		{harness.ErrSessionNotFound, protocol.CodeSessionNotFound, http.StatusNotFound},
		{harness.ErrSessionExists, protocol.CodeSessionExists, http.StatusConflict},
		{harness.ErrSessionNotOwned, protocol.CodeSessionNotOwned, http.StatusConflict},
		{harness.ErrSessionHeld, protocol.CodeSessionNotOwned, http.StatusConflict},
		{harness.ErrSessionReleased, protocol.CodeSessionNotOwned, http.StatusConflict},
		{harness.ErrSessionEnded, protocol.CodeSessionNotOwned, http.StatusConflict},
		{harness.ErrOwnershipLost, protocol.CodeSessionNotOwned, http.StatusConflict},
		{harness.ErrInputConflict, protocol.CodeInputConflict, http.StatusConflict},
		{harness.ErrTurnMismatch, protocol.CodeTurnMismatch, http.StatusConflict},
		{harness.ErrSessionBusy, protocol.CodeSessionBusy, http.StatusConflict},
		{harness.ErrRequestNotPending, protocol.CodeRequestNotPending, http.StatusConflict},
		{harness.ErrModelUnavailable, protocol.CodeModelUnavailable, http.StatusConflict},
		{harness.ErrDraining, protocol.CodeDraining, http.StatusServiceUnavailable},
		{harness.ErrBlobNotFound, protocol.CodeBlobNotFound, http.StatusNotFound},
		{harness.ErrProcessNotFound, protocol.CodeProcessNotFound, http.StatusNotFound},
		{harness.ErrUnreplayable, protocol.CodeSessionUnreplayable, http.StatusInternalServerError},
		{harness.ErrConflict, protocol.CodeSyncConflict, http.StatusInternalServerError},
		{harness.ErrStaleEpoch, protocol.CodeStaleEpoch, http.StatusInternalServerError},
		{errors.New("other"), protocol.CodeInternal, http.StatusInternalServerError},
	} {
		if got := protocol.CodeOf(tc.err); got != tc.code {
			t.Errorf("CodeOf(%v) = %q, want %q", tc.err, got, tc.code)
		}
		if got := protocol.HTTPStatus(protocol.CodeOf(tc.err)); got != tc.status {
			t.Errorf("HTTPStatus(CodeOf(%v)) = %d, want %d", tc.err, got, tc.status)
		}
	}
	if got := protocol.HTTPStatus("unknown_code"); got != http.StatusInternalServerError {
		t.Errorf("HTTPStatus(unknown) = %d, want 500", got)
	}
}

func rowALogThatDoesNotReplayFailsWithSessionUnreplayable(t *testing.T) {
	st := store.NewMemStore()
	if err := st.Append(t.Context(), "bad", 0, []byte("not an envelope")); err != nil {
		t.Fatal(err)
	}
	r := lifetimeRuntimeOn(t, st, nil)
	if _, err := r.Open(t.Context(), "bad"); !errors.Is(err, harness.ErrUnreplayable) {
		t.Errorf("Open = %v, want ErrUnreplayable", err)
	}
	status, remote := errorBodyOf(t, httpapi.Handler(r), http.MethodGet, "/sessions/bad")
	if status != http.StatusInternalServerError || !errors.Is(remote, harness.ErrUnreplayable) {
		t.Errorf("GET /sessions/bad = %d %v, want 500 and a match for ErrUnreplayable", status, remote)
	}
}

func rowNewRefusesABadOptionWithAPlainError(t *testing.T) {
	for name, opts := range map[string]harness.Options{
		"no store":              {},
		"a token without a url": {Store: store.NewMemStore(), RunToken: "t"},
		"a title_model that runs its own agent loop": {Store: store.NewMemStore(), Config: config.Config{
			Providers:  map[string]config.Provider{"cc": {Type: "claude-code-cli", BinaryPath: fakeClaudePath()}},
			TitleModel: "cc/sonnet"}},
	} {
		_, err := harness.New(opts)
		if err == nil || errors.Is(err, harness.ErrInvalidRequest) || protocol.CodeOf(err) != protocol.CodeInternal {
			t.Errorf("New with %s = %v, want a plain error", name, err)
		}
	}
}

func rowAStopCauseReachesAClientInTheErrorFrameOfTheEventStream(t *testing.T) {
	r := lifetimeRuntimeOn(t, store.NewMemStore(), nil)
	s := lifetimeSession(t, r)
	srv := httptest.NewServer(httpapi.Handler(r))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/sessions/s1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := wireClient(t, srv.Client()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	released := make(chan error, 1)
	go func() { released <- s.Release(t.Context()) }()
	var last error
	for _, err := range protocol.DecodeSSE(resp.Body) {
		if err != nil {
			last = err
			break
		}
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(last, harness.ErrSessionReleased) || !errors.Is(last, harness.ErrSessionNotOwned) || errors.Is(last, harness.ErrSessionEnded) {
		t.Errorf("stream ended with %v, want a match for ErrSessionReleased and not ErrSessionEnded", last)
	}
}

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
			Providers:           map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://model.invalid", APIKeyEnv: "HARNESS_E2E_KEY"}},
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
