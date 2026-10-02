// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// PartitionSnapshot is the most recently reported state of one table partition.
type PartitionSnapshot struct {
	TableID        int64  `json:"table_id"`
	PartitionKey   string `json:"partition_key"`
	PartitionValue string `json:"partition_value"`
	FileCount      int64  `json:"file_count"`
	TargetFileSize int64  `json:"target_file_size"`
}

// Compaction tracks one compaction task built from a partition snapshot.
type Compaction struct {
	ID             int64  `json:"id"`
	TableID        int64  `json:"table_id"`
	PartitionKey   string `json:"partition_key"`
	PartitionValue string `json:"partition_value"`
	FileCount      int64  `json:"file_count"`
	TargetFileSize int64  `json:"target_file_size"`
	Status         string `json:"status"`
}

// ErrCompactionInProgress reports that a running task already covers the table and partition.
var ErrCompactionInProgress = errors.New("compaction already in progress")

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// UpsertPartition registers the table when needed and stores the snapshot, replacing any
// earlier report for the same table and partition value.
func (s *Store) UpsertPartition(snapshot PartitionSnapshot) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin partition upsert: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO tables (id) VALUES (?)`, snapshot.TableID); err != nil {
		return fmt.Errorf("register table: %w", err)
	}
	if _, err := tx.Exec(`
INSERT INTO partitions (table_id, partition_value, partition_key, file_count, target_file_size)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (table_id, partition_value) DO UPDATE SET
	partition_key = excluded.partition_key,
	file_count = excluded.file_count,
	target_file_size = excluded.target_file_size`,
		snapshot.TableID, snapshot.PartitionValue, snapshot.PartitionKey, snapshot.FileCount, snapshot.TargetFileSize); err != nil {
		return fmt.Errorf("upsert partition: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit partition upsert: %w", err)
	}
	return nil
}

// TableExists reports whether the table number has been registered.
func (s *Store) TableExists(id int64) (bool, error) {
	var marker int
	err := s.db.QueryRow(`SELECT 1 FROM tables WHERE id = ?`, id).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check table: %w", err)
	}
	return true, nil
}

// GetPartition loads the snapshot for one table and partition value.
func (s *Store) GetPartition(tableID int64, partitionValue string) (PartitionSnapshot, bool, error) {
	row := s.db.QueryRow(`
SELECT table_id, partition_key, partition_value, file_count, target_file_size
FROM partitions WHERE table_id = ? AND partition_value = ?`, tableID, partitionValue)
	var snapshot PartitionSnapshot
	err := row.Scan(&snapshot.TableID, &snapshot.PartitionKey, &snapshot.PartitionValue, &snapshot.FileCount, &snapshot.TargetFileSize)
	if errors.Is(err, sql.ErrNoRows) {
		return PartitionSnapshot{}, false, nil
	}
	if err != nil {
		return PartitionSnapshot{}, false, fmt.Errorf("get partition: %w", err)
	}
	return snapshot, true, nil
}

// ListPartitions returns every snapshot for the table ordered by partition value ascending.
func (s *Store) ListPartitions(tableID int64) ([]PartitionSnapshot, error) {
	rows, err := s.db.Query(`
SELECT table_id, partition_key, partition_value, file_count, target_file_size
FROM partitions WHERE table_id = ? ORDER BY partition_value ASC`, tableID)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	defer rows.Close()
	var snapshots []PartitionSnapshot
	for rows.Next() {
		var snapshot PartitionSnapshot
		if err := rows.Scan(&snapshot.TableID, &snapshot.PartitionKey, &snapshot.PartitionValue, &snapshot.FileCount, &snapshot.TargetFileSize); err != nil {
			return nil, fmt.Errorf("scan partition: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate partitions: %w", err)
	}
	return snapshots, nil
}

// CreateCompaction stores a new running task built from the snapshot. It returns
// ErrCompactionInProgress when a running task already exists for the table and partition,
// in which case nothing is inserted or changed.
func (s *Store) CreateCompaction(snapshot PartitionSnapshot) (Compaction, error) {
	res, err := s.db.Exec(`
INSERT INTO compactions (table_id, partition_value, partition_key, file_count, target_file_size, status)
SELECT ?, ?, ?, ?, ?, 'running'
WHERE NOT EXISTS (
	SELECT 1 FROM compactions
	WHERE table_id = ? AND partition_value = ? AND status = 'running'
)`,
		snapshot.TableID, snapshot.PartitionValue, snapshot.PartitionKey, snapshot.FileCount, snapshot.TargetFileSize,
		snapshot.TableID, snapshot.PartitionValue)
	if err != nil {
		return Compaction{}, fmt.Errorf("insert compaction: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Compaction{}, fmt.Errorf("compaction rows affected: %w", err)
	}
	if affected == 0 {
		return Compaction{}, ErrCompactionInProgress
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Compaction{}, fmt.Errorf("compaction id: %w", err)
	}
	return Compaction{
		ID:             id,
		TableID:        snapshot.TableID,
		PartitionKey:   snapshot.PartitionKey,
		PartitionValue: snapshot.PartitionValue,
		FileCount:      snapshot.FileCount,
		TargetFileSize: snapshot.TargetFileSize,
		Status:         "running",
	}, nil
}

// GetCompaction loads one task by id without changing its state.
func (s *Store) GetCompaction(id int64) (Compaction, bool, error) {
	row := s.db.QueryRow(`
SELECT id, table_id, partition_key, partition_value, file_count, target_file_size, status
FROM compactions WHERE id = ?`, id)
	var task Compaction
	err := row.Scan(&task.ID, &task.TableID, &task.PartitionKey, &task.PartitionValue, &task.FileCount, &task.TargetFileSize, &task.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Compaction{}, false, nil
	}
	if err != nil {
		return Compaction{}, false, fmt.Errorf("get compaction: %w", err)
	}
	return task, true, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tables (
	id INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS partitions (
	table_id         INTEGER NOT NULL REFERENCES tables (id),
	partition_value  TEXT NOT NULL,
	partition_key    TEXT NOT NULL,
	file_count       INTEGER NOT NULL,
	target_file_size INTEGER NOT NULL,
	PRIMARY KEY (table_id, partition_value)
);
CREATE TABLE IF NOT EXISTS compactions (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	table_id         INTEGER NOT NULL,
	partition_value  TEXT NOT NULL,
	partition_key    TEXT NOT NULL,
	file_count       INTEGER NOT NULL,
	target_file_size INTEGER NOT NULL,
	status           TEXT NOT NULL
);
`
