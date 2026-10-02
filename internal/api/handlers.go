package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

// partitionRequest uses pointers so a missing field is told apart from an explicit zero.
type partitionRequest struct {
	PartitionKey   *string `json:"partition_key"`
	PartitionValue *string `json:"partition_value"`
	FileCount      *int64  `json:"file_count"`
	TargetFileSize *int64  `json:"target_file_size"`
}

type compactionRequest struct {
	TableID        *int64  `json:"table_id"`
	PartitionValue *string `json:"partition_value"`
}

type adviceEntry struct {
	PartitionKey   string `json:"partition_key"`
	PartitionValue string `json:"partition_value"`
	FileCount      int64  `json:"file_count"`
	TargetFileSize int64  `json:"target_file_size"`
	Action         string `json:"action"`
	Reason         string `json:"reason"`
}

func upsertPartitionHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tableID, ok := parseTableID(c)
		if !ok {
			return
		}
		var req partitionRequest
		if !bindJSON(c, &req) {
			return
		}
		var missing []string
		if req.PartitionKey == nil {
			missing = append(missing, "partition_key")
		}
		if req.PartitionValue == nil {
			missing = append(missing, "partition_value")
		}
		if req.FileCount == nil {
			missing = append(missing, "file_count")
		}
		if req.TargetFileSize == nil {
			missing = append(missing, "target_file_size")
		}
		if len(missing) > 0 {
			writeError(c, http.StatusBadRequest, "invalid_input", "missing required field(s): "+strings.Join(missing, ", "))
			return
		}
		if *req.FileCount < 0 {
			writeError(c, http.StatusBadRequest, "invalid_input", "file_count must be greater than or equal to 0")
			return
		}
		if *req.TargetFileSize <= 0 {
			writeError(c, http.StatusBadRequest, "invalid_input", "target_file_size must be greater than 0")
			return
		}
		snapshot := store.PartitionSnapshot{
			TableID:        tableID,
			PartitionKey:   *req.PartitionKey,
			PartitionValue: *req.PartitionValue,
			FileCount:      *req.FileCount,
			TargetFileSize: *req.TargetFileSize,
		}
		if err := st.UpsertPartition(snapshot); err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		c.JSON(http.StatusOK, snapshot)
	}
}

func adviceHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tableID, ok := parseTableID(c)
		if !ok {
			return
		}
		exists, err := st.TableExists(tableID)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !exists {
			writeError(c, http.StatusNotFound, "table_not_found", "table not found")
			return
		}
		snapshots, err := st.ListPartitions(tableID)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		advice := make([]adviceEntry, 0, len(snapshots))
		for _, snapshot := range snapshots {
			entry := adviceEntry{
				PartitionKey:   snapshot.PartitionKey,
				PartitionValue: snapshot.PartitionValue,
				FileCount:      snapshot.FileCount,
				TargetFileSize: snapshot.TargetFileSize,
			}
			switch {
			case snapshot.FileCount == 0:
				entry.Action = "no_compaction"
				entry.Reason = "empty_partition"
			case snapshot.FileCount == 1:
				entry.Action = "no_compaction"
				entry.Reason = "already_compact"
			default:
				entry.Action = "compact"
				entry.Reason = "multiple_files"
			}
			advice = append(advice, entry)
		}
		c.JSON(http.StatusOK, gin.H{"table_id": tableID, "advice": advice})
	}
}

func createCompactionHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req compactionRequest
		if !bindJSON(c, &req) {
			return
		}
		var missing []string
		if req.TableID == nil {
			missing = append(missing, "table_id")
		}
		if req.PartitionValue == nil {
			missing = append(missing, "partition_value")
		}
		if len(missing) > 0 {
			writeError(c, http.StatusBadRequest, "invalid_input", "missing required field(s): "+strings.Join(missing, ", "))
			return
		}
		exists, err := st.TableExists(*req.TableID)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !exists {
			writeError(c, http.StatusNotFound, "table_not_found", "table not found")
			return
		}
		snapshot, found, err := st.GetPartition(*req.TableID, *req.PartitionValue)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !found {
			writeError(c, http.StatusNotFound, "partition_not_found", "partition not found")
			return
		}
		task, err := st.CreateCompaction(snapshot)
		if errors.Is(err, store.ErrCompactionInProgress) {
			writeError(c, http.StatusConflict, "compaction_in_progress", "a compaction task is already running for this table and partition")
			return
		}
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		c.JSON(http.StatusCreated, task)
	}
}

func getCompactionHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			writeError(c, http.StatusNotFound, "compaction_not_found", "compaction not found")
			return
		}
		task, found, err := st.GetCompaction(id)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !found {
			writeError(c, http.StatusNotFound, "compaction_not_found", "compaction not found")
			return
		}
		c.JSON(http.StatusOK, task)
	}
}

// parseTableID reads the numeric table number from the path.
func parseTableID(c *gin.Context) (int64, bool) {
	tableID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_input", "table id must be an integer")
		return 0, false
	}
	return tableID, true
}

// bindJSON decodes the request body and reports type or syntax problems as invalid_input.
// An empty body is left to the required-field checks so the response can name what is missing.
func bindJSON(c *gin.Context, target any) bool {
	if err := json.NewDecoder(c.Request.Body).Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			writeError(c, http.StatusBadRequest, "invalid_input", fmt.Sprintf("field %s must be %s", typeErr.Field, jsonKindName(typeErr.Type)))
			return false
		}
		writeError(c, http.StatusBadRequest, "invalid_input", "request body must be valid JSON")
		return false
	}
	return true
}

func jsonKindName(t reflect.Type) string {
	if t.Kind() == reflect.String {
		return "a string"
	}
	return "a number"
}
