package engine_test

import (
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/engine/storetest"
)

func TestStoreConformanceDisk(t *testing.T) {
	storetest.Run(t, func(t *testing.T) engine.SessionStore {
		return engine.NewDiskStore(t.TempDir(), engine.DiskStoreOptions{})
	})
}
