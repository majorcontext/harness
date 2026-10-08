package gates

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

var probeMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

func readSpec(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func routeKey(r server.Route) string { return r.Method + " " + r.Path }

func mounted(h http.Handler, method, path string) bool {
	path = strings.NewReplacer("{id}", "x", "{input}", "x", "{request}", "x", "{name}", "x", "{key}", "x").Replace(path)
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body protocol.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	miss := body.Error.Code == protocol.CodeInvalidRequest &&
		(body.Error.Message == http.StatusText(http.StatusNotFound) || body.Error.Message == http.StatusText(http.StatusMethodNotAllowed))
	return !miss
}

func checkMounted(t *testing.T, name string, h http.Handler, want []string) {
	t.Helper()
	paths := map[string]bool{}
	for _, route := range want {
		_, path, _ := strings.Cut(route, " ")
		paths[path] = true
	}
	for path := range paths {
		for _, m := range probeMethods {
			if got, listed := mounted(h, m, path), slices.Contains(want, m+" "+path); got != listed {
				t.Errorf("%s: %s %s mounted = %v, and the spec lists it = %v", name, m, path, got, listed)
			}
		}
	}
}

func TestMountedRoutesAreTheRoutesOfTheSpec(t *testing.T) {
	spec := readSpec(t)
	want, err := SpecRoutes(spec)
	if err != nil {
		t.Fatal(err)
	}
	read, err := SpecReadRoutes(spec)
	if err != nil {
		t.Fatal(err)
	}
	var table, tableReads []string
	for _, r := range server.Table {
		table = append(table, routeKey(r))
		if r.Read {
			tableReads = append(tableReads, routeKey(r))
		}
	}
	for name, pair := range map[string][2][]string{"route table": {table, want}, "read routes of the table": {tableReads, read}} {
		got := slices.Compact(slices.Sorted(slices.Values(pair[0])))
		for _, r := range pair[1] {
			if !slices.Contains(got, r) {
				t.Errorf("the spec lists %s and the %s lacks it", r, name)
			}
		}
		for _, r := range got {
			if !slices.Contains(pair[1], r) {
				t.Errorf("the %s holds %s and the spec does not list it", name, r)
			}
		}
	}
	rt, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close(t.Context()) })
	checkMounted(t, "Runtime.Handler", rt.Handler(), want)
	checkMounted(t, "ReadHandler", harness.ReadHandler(harness.NewMemStore(), nil), read)
}

func TestSpecListsParse(t *testing.T) {
	const spec = "### Routes\n\n```\nGET /a/{id}  note\nGET /p · POST /p/{name}/{action}\n```\n\n" +
		"`harness.ReadHandler(store, queue)` is the HTTP form of reads. It serves `GET /a/{id}` through the code of x.\n\n" +
		"`start`, `stop`, and `restart` reply with the status.\n\n### Public packages\n\n| Package | Owns |\n| --- | --- |\n| `harness` | x |\n| `harness/config` | y |\n\n### Next\n| `harness/other` | z |\n"
	routes, err := SpecRoutes(spec)
	if err != nil || strings.Join(routes, ",") != "GET /a/{id},GET /p,POST /p/{name}/restart,POST /p/{name}/start,POST /p/{name}/stop" {
		t.Errorf("SpecRoutes = %v, %v", routes, err)
	}
	read, err := SpecReadRoutes(spec)
	if err != nil || strings.Join(read, ",") != "GET /a/{id}" {
		t.Errorf("SpecReadRoutes = %v, %v", read, err)
	}
	pkgs, err := SpecPublicPackages(spec)
	if err != nil || strings.Join(pkgs, ",") != ",config" {
		t.Errorf("SpecPublicPackages = %q, %v", pkgs, err)
	}
}
