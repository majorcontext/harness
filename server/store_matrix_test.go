package server

import (
	"os"
	"sync"
	"testing"

	"github.com/majorcontext/harness/engine"
)

func memStoreMode() bool { return os.Getenv("HARNESS_TEST_STORE") == "mem" }

var (
	memStoresMu sync.Mutex
	memStores   = map[string]*engine.MemStore{}
)

// testStore returns the store a test server and its engine sessions share for
// dir: nil on the disk matrix, one memory store per dir on the mem matrix.
func testStore(dir string) engine.SessionStore {
	if !memStoreMode() || dir == "" {
		return nil
	}
	memStoresMu.Lock()
	defer memStoresMu.Unlock()
	st, ok := memStores[dir]
	if !ok {
		st = engine.NewMemStore()
		memStores[dir] = st
	}
	return st
}

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	if memStoreMode() {
		return Options{Store: testStore(dir)}
	}
	return Options{SessionDir: dir}
}

func requireDiskStore(t *testing.T) {
	t.Helper()
	if memStoreMode() {
		t.Skip("reads files directly")
	}
}
