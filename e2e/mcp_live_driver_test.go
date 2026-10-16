package e2e

import (
	"net/http"
	"testing"
)

func (d *runtimeDriver) PutMCPServers(t *testing.T, body any) callResult {
	t.Helper()
	return d.call(t, http.MethodPut, "/mcp/servers", body)
}
