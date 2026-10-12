package store_test

import (
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/store"
	"github.com/majorcontext/harness/storetest"
)

func TestMemStore(t *testing.T) {
	storetest.Run(t, func(*testing.T) harness.Store { return store.NewMemStore() })
	storetest.RunInstances(t, func(*testing.T) func() harness.Store {
		st := store.NewMemStore()
		return func() harness.Store { return st }
	})
}
