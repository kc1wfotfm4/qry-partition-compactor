package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesUsableStore(t *testing.T) {
	st := openTestStore(t)
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestUpsertPartitionRegistersTableAndUpdatesSnapshot(t *testing.T) {
	st := openTestStore(t)

	exists, err := st.TableExists(7)
	if err != nil {
		t.Fatalf("table exists: %v", err)
	}
	if exists {
		t.Fatal("table 7 should not exist before any report")
	}

	first := Partition{TableID: 7, PartitionKey: "dt", PartitionValue: "2026-10-01", FileCount: 3, TargetFileSize: 128}
	if err := st.UpsertPartition(first); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	exists, err = st.TableExists(7)
	if err != nil {
		t.Fatalf("table exists: %v", err)
	}
	if !exists {
		t.Fatal("table 7 should be registered by the report")
	}

	updated := Partition{TableID: 7, PartitionKey: "dt", PartitionValue: "2026-10-01", FileCount: 9, TargetFileSize: 256}
	if err := st.UpsertPartition(updated); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	partitions, err := st.ListPartitions(7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(partitions) != 1 {
		t.Fatalf("len(partitions) = %d, want 1", len(partitions))
	}
	if partitions[0].FileCount != 9 || partitions[0].TargetFileSize != 256 {
		t.Fatalf("snapshot = %+v, want updated file_count 9 and target_file_size 256", partitions[0])
	}
}

func TestListPartitionsOrdersByPartitionValue(t *testing.T) {
	st := openTestStore(t)
	for _, value := range []string{"2026-10-03", "2026-10-01", "2026-10-02"} {
		p := Partition{TableID: 3, PartitionKey: "dt", PartitionValue: value, FileCount: 2, TargetFileSize: 64}
		if err := st.UpsertPartition(p); err != nil {
			t.Fatalf("upsert %s: %v", value, err)
		}
	}
	partitions, err := st.ListPartitions(3)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"2026-10-01", "2026-10-02", "2026-10-03"}
	if len(partitions) != len(want) {
		t.Fatalf("len(partitions) = %d, want %d", len(partitions), len(want))
	}
	for i, value := range want {
		if partitions[i].PartitionValue != value {
			t.Fatalf("partitions[%d].PartitionValue = %s, want %s", i, partitions[i].PartitionValue, value)
		}
	}
}

func TestCreateCompactionLifecycle(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateCompaction(1, "2026-10-01"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("err = %v, want ErrTableNotFound", err)
	}

	p := Partition{TableID: 1, PartitionKey: "dt", PartitionValue: "2026-10-01", FileCount: 4, TargetFileSize: 512}
	if err := st.UpsertPartition(p); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if _, err := st.CreateCompaction(1, "2026-10-02"); !errors.Is(err, ErrPartitionNotFound) {
		t.Fatalf("err = %v, want ErrPartitionNotFound", err)
	}

	created, err := st.CreateCompaction(1, "2026-10-01")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("created task should have a non-zero id")
	}
	if created.Status != StatusRunning {
		t.Fatalf("status = %s, want %s", created.Status, StatusRunning)
	}
	if created.PartitionKey != "dt" || created.FileCount != 4 || created.TargetFileSize != 512 {
		t.Fatalf("snapshot = %+v, want the reported partition fields", created)
	}

	if _, err := st.CreateCompaction(1, "2026-10-01"); !errors.Is(err, ErrCompactionInProgress) {
		t.Fatalf("err = %v, want ErrCompactionInProgress", err)
	}

	loaded, found, err := st.GetCompaction(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("created task should be loadable")
	}
	if loaded != created {
		t.Fatalf("loaded = %+v, want %+v", loaded, created)
	}

	if _, found, err := st.GetCompaction(created.ID + 1000); err != nil || found {
		t.Fatalf("unknown id: found = %v, err = %v, want false and nil", found, err)
	}
}
