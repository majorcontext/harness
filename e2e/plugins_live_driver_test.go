package e2e

import (
	"net/http"
	"testing"
)

func (d *runtimeDriver) PutPlugins(t *testing.T, body any) callResult {
	t.Helper()
	return d.call(t, http.MethodPut, "/plugins", body)
}
