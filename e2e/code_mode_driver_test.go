package e2e

import (
	"net/http"
	"testing"
)

func (d *runtimeDriver) CreateCodeMode(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/sessions", map[string]any{"code_mode": true})
}

func (d *runtimeDriver) SetCodeMode(t *testing.T, id string, on bool) callResult {
	t.Helper()
	return d.call(t, http.MethodPatch, "/sessions/"+id, map[string]any{"code_mode": on})
}
