package harness_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
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
