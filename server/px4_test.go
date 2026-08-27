package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"testing"
)

func TestProbeDirect(t *testing.T) {
	if err := pprof.StartCPUProfile(io.Discard); err != nil { t.Fatal(err) }
	defer pprof.StopCPUProfile()
	w := httptest.NewRecorder()
	serveCPUProfile(w, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=1", nil))
	t.Logf("code=%d hdr=%v body=%q", w.Code, w.Header(), w.Body.String())
}
