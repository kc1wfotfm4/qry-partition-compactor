package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestPartitionUpsertUpdatesExistingRow(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	exists, err := st.TableExists(9)
	if err != nil || exists {
		t.Fatalf("TableExists(9) = %v, %v, want false, nil", exists, err)
	}

	seed := PartitionSnapshot{TableID: 9, PartitionKey: "dt", PartitionValue: "2024-01-02", FileCount: 4, TargetFileSize: 128}
	if err := st.UpsertPartition(seed); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertPartition(PartitionSnapshot{TableID: 9, PartitionKey: "dt", PartitionValue: "2024-01-01", FileCount: 1, TargetFileSize: 128}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertPartition(PartitionSnapshot{TableID: 9, PartitionKey: "dt", PartitionValue: "2024-01-02", FileCount: 7, TargetFileSize: 256}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	exists, err = st.TableExists(9)
	if err != nil || !exists {
		t.Fatalf("TableExists(9) = %v, %v, want true, nil", exists, err)
	}

	snapshots, err := st.ListPartitions(9)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2 (re-report must update, not duplicate)", len(snapshots))
	}
	if snapshots[0].PartitionValue != "2024-01-01" || snapshots[1].PartitionValue != "2024-01-02" {
		t.Fatalf("order = %s, %s, want ascending", snapshots[0].PartitionValue, snapshots[1].PartitionValue)
	}
	if snapshots[1].FileCount != 7 || snapshots[1].TargetFileSize != 256 {
		t.Fatalf("updated snapshot = %+v", snapshots[1])
	}

	loaded, found, err := st.GetPartition(9, "2024-01-02")
	if err != nil || !found || loaded.FileCount != 7 {
		t.Fatalf("GetPartition = %+v, %v, %v", loaded, found, err)
	}
	if _, found, err := st.GetPartition(9, "nope"); err != nil || found {
		t.Fatalf("GetPartition missing = %v, %v, want false, nil", found, err)
	}
}

func TestCompactionConflictAndLookup(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	snapshot := PartitionSnapshot{TableID: 3, PartitionKey: "dt", PartitionValue: "a", FileCount: 5, TargetFileSize: 64}
	if err := st.UpsertPartition(snapshot); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	task, err := st.CreateCompaction(snapshot)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.ID < 1 || task.Status != "running" || task.FileCount != 5 || task.TargetFileSize != 64 {
		t.Fatalf("task = %+v", task)
	}

	if _, err := st.CreateCompaction(snapshot); !errors.Is(err, ErrCompactionInProgress) {
		t.Fatalf("second create err = %v, want ErrCompactionInProgress", err)
	}

	loaded, found, err := st.GetCompaction(task.ID)
	if err != nil || !found {
		t.Fatalf("GetCompaction = %v, %v", found, err)
	}
	if loaded != task {
		t.Fatalf("loaded = %+v, want %+v", loaded, task)
	}
	if _, found, err := st.GetCompaction(task.ID + 100); err != nil || found {
		t.Fatalf("GetCompaction unknown = %v, %v, want false, nil", found, err)
	}
}
