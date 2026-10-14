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
	responses "github.com/majorcontext/harness/internal/provider/openai"
)

// catalogTimeout bounds the fetch of a model list.
const catalogTimeout = 3 * time.Second

// catalogBackend is a backend whose endpoint serves the list of its models. The first Models call fetches the list and keeps it. A failed or empty fetch keeps the static list.
type catalogBackend struct {
	*modelapi.Backend
	name   string
	fetch  func(ctx context.Context) ([]modelmeta.Model, error)
	static []modelmeta.Model
	log    *slog.Logger

	mu     sync.Mutex
	models []modelmeta.Model
	done   bool
}

// Models returns the models of the backend. It waits at most catalogTimeout for the endpoint.
func (b *catalogBackend) Models(ctx context.Context) []modelmeta.Model {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return b.models
	}
	fetchCtx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	got, err := b.fetch(fetchCtx)
	switch {
	case ctx.Err() != nil || rejected(err):
		return b.static
	case err != nil:
		b.log.Warn("model list fetch failed; using the static list", "provider", b.name, "error", err.Error())
	case len(got) == 0:
		b.log.Warn("model list is empty; using the static list", "provider", b.name)
	default:
		b.models = got
	}
	if b.models == nil {
		b.models = b.static
	}
	b.done = true
	return b.models
}

// rejected reports that the login was missing or refused. The list is then not kept, so a later call can use a login that appears.
func rejected(err error) bool {
	if codexauth.IsLoginRequired(err) {
		return true
	}
	var status *responses.ModelsStatusError
	return errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden)
}
