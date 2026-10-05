package harness_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

func typed(id, s string) protocol.Input {
	in := text(id, s)
	in.Source = protocol.SourceTyped
	return in
}

// commandLine renders a command.recorded record, or line for any other kind.
func commandLine(kind string, d json.RawMessage) string {
	if kind != "command.recorded" {
		return line(kind, d)
	}
	var c struct{ InputID, Name, Status, Text string }
	_ = json.Unmarshal(d, &c)
	return strings.TrimSpace(strings.Join([]string{kind, c.Name, c.Status, c.Text}, " "))
}

func TestACommandThatPanicsFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := harness.NewMemStore()
		r := runtime(t, st, newFake())
		s := create(t, r)
		harness.PanicIn(t, command.OpStatus)
		if got := submit(t, s, typed("a", "/status")); got.Command != protocol.CommandAccepted {
			t.Errorf("receipt = %+v, want command %q", got, protocol.CommandAccepted)
		}
		recs, err := st.Read(bg, "s1", 2, 100)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, rec := range recs {
			var env struct {
				K string          `json:"k"`
				D json.RawMessage `json:"d"`
			}
			if err := json.Unmarshal(rec.Data, &env); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, commandLine(env.K, env.D))
		}
		want := []string{"command.recorded status accepted", "command.recorded status failed /status failed: internal error"}
		if !slices.Equal(lines, want) {
			t.Errorf("log =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
		closeRuntime(t, r)
	})
}

func TestCommandListNamesNoRouteAndEachRouteExists(t *testing.T) {
	r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore()}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	menu, err := r.Commands()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range menu.Commands {
		if c.Method != "" || c.Path != "" {
			t.Errorf("Commands entry %s names the route %s %s; the HTTP handler names routes", c.Name, c.Method, c.Path)
		}
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/commands", nil))
	var got protocol.Commands
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /commands = %d %v", rec.Code, err)
	}
	for _, c := range got.Commands {
		if c.Path == "" {
			continue
		}
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(c.Method, strings.ReplaceAll(c.Path, "{id}", "nope"), nil))
		if rec.Code == http.StatusMethodNotAllowed || rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), protocol.CodeInvalidRequest) {
			t.Errorf("%s %s has no route: %d %s", c.Method, c.Path, rec.Code, rec.Body)
		}
	}
}

func TestTheMuxServesTheRoutesOfTheTableAndNoOtherMethod(t *testing.T) {
	r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), WorkDir: t.TempDir()}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	miss := func(method, path string) bool {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Accept", "text/event-stream")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		var body protocol.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Error.Code == protocol.CodeInvalidRequest && (body.Error.Message == http.StatusText(http.StatusNotFound) || body.Error.Message == http.StatusText(http.StatusMethodNotAllowed))
	}
	served := map[string][]string{}
	for _, route := range server.Table {
		path := strings.NewReplacer("{id}", "x", "{input}", "x", "{request}", "x", "{name}", "x").Replace(route.Path)
		served[path] = append(served[path], route.Method)
		if miss(route.Method, path) {
			t.Errorf("%s %s (%s) is in the table and not on the mux", route.Method, route.Path, route.Name)
		}
	}
	for path, methods := range served {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if !slices.Contains(methods, m) && !miss(m, path) {
				t.Errorf("%s %s is on the mux and not in the table", m, path)
			}
		}
	}
}

func TestCloseRefusesATypedCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := &held{Store: harness.NewMemStore(), hold: make(chan struct{})}, newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("busy", "work"))
		<-f.runs
		st.armed.Store(true)
		closed := make(chan error, 1)
		go func() { closed <- r.Close(bg) }()
		synctest.Wait()
		admitted := make(chan error, 1)
		go func() { _, err := s.Submit(bg, typed("a", "/thinking high")); admitted <- err }()
		synctest.Wait()
		select {
		case err := <-admitted:
			if !errors.Is(err, harness.ErrDraining) {
				t.Errorf("Submit = %v, want ErrDraining", err)
			}
		default:
			t.Error("Submit waits for the session while Close runs, want ErrDraining")
		}
		close(st.hold)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	})
}
