package e2e

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

// processPath is the path of a process route: the engine serves /process
// and the runtime serves /processes.
func processPath(root, name, rest string) string {
	if name == "" {
		return root
	}
	return root + "/" + url.PathEscape(name) + rest
}

func logsPath(root, name string, tail int) string {
	path := processPath(root, name, "/logs")
	if tail != 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	return path
}

func changesPath(root, scope, dir string) string {
	q := url.Values{}
	if scope != "" {
		q.Set("scope", scope)
	}
	if dir != "" {
		q.Set("dir", dir)
	}
	if len(q) == 0 {
		return root
	}
	return root + "?" + q.Encode()
}

func (d *httpDriver) Processes(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/process", nil)
}

func (d *httpDriver) ProcessAction(t *testing.T, name, action string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, processPath("/process", name, "/"+action), nil)
}

func (d *httpDriver) ProcessLogs(t *testing.T, name string, tail int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, logsPath("/process", name, tail), nil)
}

func (d *httpDriver) WorkspaceChanges(t *testing.T, scope, dir string) callResult {
	t.Helper()
	path := changesPath("/git/changes", scope, dir)
	resp, data := d.p.do(http.MethodGet, path, nil)
	return changesResult(t, path, resp.StatusCode, data)
}

func (d *runtimeDriver) Processes(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/processes", nil)
}

func (d *runtimeDriver) ProcessAction(t *testing.T, name, action string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, processPath("/processes", name, "/"+action), nil)
}

func (d *runtimeDriver) ProcessLogs(t *testing.T, name string, tail int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, logsPath("/processes", name, tail), nil)
}

func (d *runtimeDriver) WorkspaceChanges(t *testing.T, scope, dir string) callResult {
	t.Helper()
	path := changesPath("/workspace/changes", scope, dir)
	status, data := d.do(t, http.MethodGet, path, nil)
	return changesResult(t, path, status, data)
}

// changesResult decodes a changes reply and keeps its bytes in Wire: a
// decoded body cannot show whether the patch went out HTML-escaped.
func changesResult(t *testing.T, path string, status int, data []byte) callResult {
	t.Helper()
	return callResult{Status: status, Body: decodeBody(t, "GET "+path, data), Wire: data}
}
