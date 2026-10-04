package harness_test

import (
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/storetest"
)

func TestMemStore(t *testing.T) {
	storetest.Run(t, func(*testing.T) func() harness.Store {
		st := harness.NewMemStore()
		return func() harness.Store { return st }
	})
}
