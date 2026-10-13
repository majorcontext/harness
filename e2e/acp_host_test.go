package e2e

import (
	"io"
	"testing"

	"github.com/majorcontext/harness/internal/acp"
)

// startACPInProcess runs acp.Serve over two pipes in this process, so the
// race detector sees the adapter.
func startACPInProcess(t *testing.T) (io.WriteCloser, io.Reader, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := acp.Serve(t.Context(), inR, outW, acp.Config{Version: "in-process"})
		_ = outW.Close()
		_ = inR.CloseWithError(io.ErrClosedPipe)
		done <- err
	}()
	stop := func() {
		t.Helper()
		if err := <-done; err != nil {
			t.Errorf("acp.Serve ended with %v", err)
		}
	}
	return inW, outR, stop
}
