package harness_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

func TestAGetOfASessionNeitherOwnsItNorStartsItsTurn(t *testing.T) {
	for _, path := range []string{"/sessions/s1", "/sessions/s1/events"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := harness.NewMemStore()
				f1 := newFake()
				r1 := runtime(t, st, f1)
				s := create(t, r1)
				submit(t, s, text("a", "hi"))
				<-f1.runs
				closeRuntime(t, r1)
				head, err := st.Head(bg, "s1")
				if err != nil {
					t.Fatal(err)
				}
				f2 := newFake()
				r2 := runtime(t, st, f2)
				rec := httptest.NewRecorder()
				r2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				synctest.Wait()
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"s1"`) && path == "/sessions/s1" {
					t.Errorf("GET %s = %d %s, want 200 with the session", path, rec.Code, rec.Body)
				}
				if got, _ := st.Head(bg, "s1"); got != head {
					t.Errorf("head after the GET = %d, want %d: the GET appended to the log", got, head)
				}
				select {
				case <-f2.runs:
					t.Error("the GET resumed the turn of the session")
				default:
				}
				closeRuntime(t, r2)
			})
		})
	}
}

func TestAGetOfASessionThatTheRuntimeDoesNotRunListsThePlugins(t *testing.T) {
	st := harness.NewMemStore()
	r1 := runtime(t, st, newFake())
	create(t, r1)
	closeRuntime(t, r1)
	r2, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{Plugins: pluginFixture(t, `{}`)}}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r2) })
	if _, err := r2.Create(bg, protocol.CreateSession{ID: "s2", Model: "fake/model"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1", nil))
	var got protocol.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Plugins) != 1 || got.Plugins[0].Name != "fixture" {
		t.Errorf("GET of a cold session = %d %s (%v), want the fixture plugin", rec.Code, rec.Body, err)
	}
}
