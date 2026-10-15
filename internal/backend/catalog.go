package backend

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/provider"
	responses "github.com/majorcontext/harness/internal/provider/openai"
)

const (
	// catalogTimeout bounds one fetch of a model list.
	catalogTimeout = 3 * time.Second
	// catalogRetry is how long a failed fetch keeps the static list before the next call tries again.
	catalogRetry = 5 * time.Minute
)

// catalogBackend is a backend whose endpoint serves the list of its models. The first Models call starts a fetch that all callers share.
// A list is kept for the life of the process. A failed or empty fetch gives the static list for catalogRetry.
// A refused login gives the static list until the credential changes.
type catalogBackend struct {
	*modelapi.Backend
	name   string
	fetch  func(ctx context.Context) ([]modelmeta.Model, error)
	static []modelmeta.Model
	log    *slog.Logger
	// credential returns the access token of the login now, or empty. It reads no network.
	credential func() string
	// refusedRetry, when set, is how long a refused list keeps the static list before the next call tries again, whatever credential returns.
	refusedRetry time.Duration

	mu          sync.Mutex
	models      []modelmeta.Model
	retryAt     time.Time
	refused     bool
	refusedWith string
	flight      chan struct{}
}

// Models returns the models of the backend. It waits at most catalogTimeout for the endpoint, and returns the static list when ctx ends first.
func (b *catalogBackend) Models(ctx context.Context) []modelmeta.Model {
	b.mu.Lock()
	switch {
	case b.models != nil:
		defer b.mu.Unlock()
		return b.models
	case !b.refused && time.Now().Before(b.retryAt), b.refused && b.credential() == b.refusedWith && (b.refusedRetry == 0 || time.Now().Before(b.retryAt)):
		b.mu.Unlock()
		return b.static
	}
	done := b.flight
	if done == nil {
		done = make(chan struct{})
		b.flight = done
		go b.run(done)
	}
	b.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return b.static
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.models != nil {
		return b.models
	}
	return b.static
}

// run fetches the list once and records the outcome, then wakes the callers that wait on done.
func (b *catalogBackend) run(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	got, err := b.fetch(ctx)
	credential := b.credential()
	b.mu.Lock()
	defer b.mu.Unlock()
	defer close(done)
	b.flight = nil
	switch {
	case rejected(err):
		b.refused, b.refusedWith = true, credential
		if b.refusedRetry > 0 {
			b.retryAt = time.Now().Add(b.refusedRetry)
		}
	case err != nil:
		b.refused = false
		b.retryAt = time.Now().Add(catalogRetry)
		b.log.Warn("model list fetch failed; using the static list", "provider", b.name, "error", err.Error())
	case len(got) == 0:
		b.refused = false
		b.retryAt = time.Now().Add(catalogRetry)
		b.log.Warn("model list is empty; using the static list", "provider", b.name)
	default:
		b.models, b.refused = got, false
	}
}

// rejected reports that the login was missing or refused.
func rejected(err error) bool {
	if codexauth.IsLoginRequired(err) || provider.AsLoginRequired(err) {
		return true
	}
	var status *responses.ModelsStatusError
	return errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden)
}
