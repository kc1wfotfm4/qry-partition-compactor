// Package store owns the SQLite file and every write the service performs.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Partition is the latest reported snapshot for one table partition.
type Partition struct {
	TableID        int64
	PartitionKey   string
	PartitionValue string
	FileCount      int64
	TargetFileSize int64
}

// Compaction is a tracked compaction task plus the partition snapshot it was created from.
type Compaction struct {
	ID             int64
	TableID        int64
	PartitionKey   string
	PartitionValue string
	FileCount      int64
	TargetFileSize int64
	Status         string
}

// StatusRunning marks a compaction task that has not finished yet.
const StatusRunning = "running"

// Domain errors the API layer translates into published error codes.
var (
	ErrTableNotFound        = errors.New("table not found")
	ErrPartitionNotFound    = errors.New("partition not found")
	ErrCompactionInProgress = errors.New("compaction already in progress")
)

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
// earlier snapshot reported for the same table and partition value.
func (s *Store) UpsertPartition(p Partition) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin upsert partition: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tables (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, p.TableID); err != nil {
		return fmt.Errorf("register table: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO partitions (table_id, partition_key, partition_value, file_count, target_file_size)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (table_id, partition_value) DO UPDATE SET
			partition_key = excluded.partition_key,
			file_count = excluded.file_count,
			target_file_size = excluded.target_file_size`,
		p.TableID, p.PartitionKey, p.PartitionValue, p.FileCount, p.TargetFileSize); err != nil {
		return fmt.Errorf("upsert partition: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert partition: %w", err)
	}
	return nil
}

// TableExists reports whether any partition report has registered the table.
func (s *Store) TableExists(tableID int64) (bool, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM tables WHERE id = ?`, tableID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check table: %w", err)
	}
	return true, nil
}

// ListPartitions returns every snapshot for the table ordered by partition value ascending.
func (s *Store) ListPartitions(tableID int64) ([]Partition, error) {
	rows, err := s.db.Query(`SELECT table_id, partition_key, partition_value, file_count, target_file_size
		FROM partitions WHERE table_id = ? ORDER BY partition_value ASC`, tableID)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	defer rows.Close()
	partitions := make([]Partition, 0)
	for rows.Next() {
		var p Partition
		if err := rows.Scan(&p.TableID, &p.PartitionKey, &p.PartitionValue, &p.FileCount, &p.TargetFileSize); err != nil {
			return nil, fmt.Errorf("scan partition: %w", err)
		}
		partitions = append(partitions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate partitions: %w", err)
	}
	return partitions, nil
}

// CreateCompaction stores a new running task built from the current partition snapshot. It
// fails with ErrTableNotFound, ErrPartitionNotFound, or ErrCompactionInProgress without
// writing anything when the request cannot be accepted.
func (s *Store) CreateCompaction(tableID int64, partitionValue string) (Compaction, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Compaction{}, fmt.Errorf("begin create compaction: %w", err)
	}
	defer tx.Rollback()

	var table int64
	err = tx.QueryRow(`SELECT id FROM tables WHERE id = ?`, tableID).Scan(&table)
	if errors.Is(err, sql.ErrNoRows) {
		return Compaction{}, ErrTableNotFound
	}
	if err != nil {
		return Compaction{}, fmt.Errorf("check table: %w", err)
	}

	var c Compaction
	err = tx.QueryRow(`SELECT partition_key, file_count, target_file_size
		FROM partitions WHERE table_id = ? AND partition_value = ?`, tableID, partitionValue).
		Scan(&c.PartitionKey, &c.FileCount, &c.TargetFileSize)
	if errors.Is(err, sql.ErrNoRows) {
		return Compaction{}, ErrPartitionNotFound
	}
	if err != nil {
		return Compaction{}, fmt.Errorf("load partition: %w", err)
	}

	var runningID int64
	err = tx.QueryRow(`SELECT id FROM compactions WHERE table_id = ? AND partition_value = ? AND status = ?`,
		tableID, partitionValue, StatusRunning).Scan(&runningID)
	if err == nil {
		return Compaction{}, ErrCompactionInProgress
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Compaction{}, fmt.Errorf("check running compaction: %w", err)
	}

	result, err := tx.Exec(`INSERT INTO compactions (table_id, partition_key, partition_value, file_count, target_file_size, status)
		VALUES (?, ?, ?, ?, ?, ?)`, tableID, c.PartitionKey, partitionValue, c.FileCount, c.TargetFileSize, StatusRunning)
	if err != nil {
		if isUniqueViolation(err) {
			return Compaction{}, ErrCompactionInProgress
		}
		return Compaction{}, fmt.Errorf("insert compaction: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Compaction{}, fmt.Errorf("read compaction id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Compaction{}, fmt.Errorf("commit create compaction: %w", err)
	}
	c.ID = id
	c.TableID = tableID
	c.PartitionValue = partitionValue
	c.Status = StatusRunning
	return c, nil
}

// GetCompaction returns the stored task; found is false for unknown ids.
func (s *Store) GetCompaction(id int64) (Compaction, bool, error) {
	var c Compaction
	err := s.db.QueryRow(`SELECT id, table_id, partition_key, partition_value, file_count, target_file_size, status
		FROM compactions WHERE id = ?`, id).
		Scan(&c.ID, &c.TableID, &c.PartitionKey, &c.PartitionValue, &c.FileCount, &c.TargetFileSize, &c.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Compaction{}, false, nil
	}
	if err != nil {
		return Compaction{}, false, fmt.Errorf("load compaction: %w", err)
	}
	return c, true, nil
}

// isUniqueViolation reports whether err is the SQLite unique-constraint error raised when a
// concurrent insert races the running-task check.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() == 2067 // SQLITE_CONSTRAINT_UNIQUE
	}
	return false
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
	table_id         INTEGER NOT NULL,
	partition_key    TEXT NOT NULL,
	partition_value  TEXT NOT NULL,
	file_count       INTEGER NOT NULL,
	target_file_size INTEGER NOT NULL,
	PRIMARY KEY (table_id, partition_value)
);

CREATE TABLE IF NOT EXISTS compactions (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	table_id         INTEGER NOT NULL,
	partition_key    TEXT NOT NULL,
	partition_value  TEXT NOT NULL,
	file_count       INTEGER NOT NULL,
	target_file_size INTEGER NOT NULL,
	status           TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS compactions_one_running
	ON compactions (table_id, partition_value) WHERE status = 'running';
`
