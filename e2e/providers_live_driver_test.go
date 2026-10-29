package e2e

import (
	"net/http"
	"testing"
)

func (d *runtimeDriver) PutProviders(t *testing.T, body any) callResult {
	t.Helper()
	return d.call(t, http.MethodPut, "/providers", body)
}

func (d *runtimeDriver) ReconnectProvider(t *testing.T, name string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/providers/"+name+"/reconnect", nil)
}
