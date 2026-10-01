package engine

import "testing"

func TestSnapshotGateFollowsStore(t *testing.T) {
	mem := NewSession(Config{SessionStore: NewMemStore(), SnapshotEveryRecords: 1})
	if mem.snapshotEnabled() {
		t.Error("snapshotEnabled on a MemStore session = true, want false")
	}
	disk := NewSession(Config{SessionStore: NewDiskStore(t.TempDir(), DiskStoreOptions{}), SnapshotEveryRecords: 1})
	if !disk.snapshotEnabled() {
		t.Error("snapshotEnabled on a DiskStore session = false, want true")
	}
}
