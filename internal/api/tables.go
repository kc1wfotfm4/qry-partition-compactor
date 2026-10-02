package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

// partitionReport is the accepted body for POST /api/v1/tables/{id}/partitions. Pointer fields
// let the handler tell a missing field apart from an explicit zero value.
type partitionReport struct {
	PartitionKey   *string `json:"partition_key"`
	PartitionValue *string `json:"partition_value"`
	FileCount      *int64  `json:"file_count"`
	TargetFileSize *int64  `json:"target_file_size"`
}

func handleUpsertPartition(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tableID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_input", "table id must be an integer")
			return
		}
		var report partitionReport
		if err := decodeBody(c, &report); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		if report.PartitionKey == nil || *report.PartitionKey == "" {
			writeError(c, http.StatusBadRequest, "invalid_input", "partition_key is required")
			return
		}
		if report.PartitionValue == nil || *report.PartitionValue == "" {
			writeError(c, http.StatusBadRequest, "invalid_input", "partition_value is required")
			return
		}
		if report.FileCount == nil {
			writeError(c, http.StatusBadRequest, "invalid_input", "file_count is required")
			return
		}
		if *report.FileCount < 0 {
			writeError(c, http.StatusBadRequest, "invalid_input", "file_count must be greater than or equal to 0")
			return
		}
		if report.TargetFileSize == nil {
			writeError(c, http.StatusBadRequest, "invalid_input", "target_file_size is required")
			return
		}
		if *report.TargetFileSize <= 0 {
			writeError(c, http.StatusBadRequest, "invalid_input", "target_file_size must be greater than 0")
			return
		}
		partition := store.Partition{
			TableID:        tableID,
			PartitionKey:   *report.PartitionKey,
			PartitionValue: *report.PartitionValue,
			FileCount:      *report.FileCount,
			TargetFileSize: *report.TargetFileSize,
		}
		if err := st.UpsertPartition(partition); err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
			return
		}
		c.JSON(http.StatusOK, partitionSnapshot(partition))
	}
}

// partitionSnapshot renders the stored partition report.
func partitionSnapshot(p store.Partition) gin.H {
	return gin.H{
		"table_id":         p.TableID,
		"partition_key":    p.PartitionKey,
		"partition_value":  p.PartitionValue,
		"file_count":       p.FileCount,
		"target_file_size": p.TargetFileSize,
	}
}

// adviceEntry is one partition's compaction recommendation.
type adviceEntry struct {
	PartitionKey   string `json:"partition_key"`
	PartitionValue string `json:"partition_value"`
	FileCount      int64  `json:"file_count"`
	TargetFileSize int64  `json:"target_file_size"`
	Action         string `json:"action"`
	Reason         string `json:"reason"`
}

func handleListAdvice(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tableID, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			writeError(c, http.StatusNotFound, "table_not_found", "table not found")
			return
		}
		exists, err := st.TableExists(tableID)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
			return
		}
		if !exists {
			writeError(c, http.StatusNotFound, "table_not_found", "table not found")
			return
		}
		partitions, err := st.ListPartitions(tableID)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
			return
		}
		advice := make([]adviceEntry, 0, len(partitions))
		for _, p := range partitions {
			advice = append(advice, advisePartition(p))
		}
		c.JSON(http.StatusOK, gin.H{"table_id": tableID, "advice": advice})
	}
}

// advisePartition maps a snapshot to its recommendation without touching any task state.
func advisePartition(p store.Partition) adviceEntry {
	entry := adviceEntry{
		PartitionKey:   p.PartitionKey,
		PartitionValue: p.PartitionValue,
		FileCount:      p.FileCount,
		TargetFileSize: p.TargetFileSize,
	}
	switch {
	case p.FileCount == 0:
		entry.Action = "no_compaction"
		entry.Reason = "empty_partition"
	case p.FileCount == 1:
		entry.Action = "no_compaction"
		entry.Reason = "already_compact"
	default:
		entry.Action = "compact"
		entry.Reason = "multiple_files"
	}
	return entry
}
