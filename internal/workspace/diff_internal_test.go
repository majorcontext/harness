package workspace

import (
	"strings"
	"testing"
)

func TestLastWholeFileBoundaryKeepsAFileThatEndsAtTheLimit(t *testing.T) {
	const limit = 20
	data := []byte(strings.Repeat("a", limit-1) + "\ndiff --git a/b b/b\n")
	if got := lastWholeFileBoundary(data, limit); got != limit {
		t.Errorf("lastWholeFileBoundary = %d, want %d", got, limit)
	}
}
