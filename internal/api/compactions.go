package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

// compactionRequest is the accepted body for POST /api/v1/compactions.
type compactionRequest struct {
	TableID        *int64  `json:"table_id"`
	PartitionValue *string `json:"partition_value"`
}

func handleCreateCompaction(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req compactionRequest
		if err := decodeBody(c, &req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		if req.TableID == nil {
			writeError(c, http.StatusBadRequest, "invalid_input", "table_id is required")
			return
		}
		if req.PartitionValue == nil || *req.PartitionValue == "" {
			writeError(c, http.StatusBadRequest, "invalid_input", "partition_value is required")
			return
		}
		compaction, err := st.CreateCompaction(*req.TableID, *req.PartitionValue)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrTableNotFound):
				writeError(c, http.StatusNotFound, "table_not_found", "table not found")
			case errors.Is(err, store.ErrPartitionNotFound):
				writeError(c, http.StatusNotFound, "partition_not_found", "partition not found")
			case errors.Is(err, store.ErrCompactionInProgress):
				writeError(c, http.StatusConflict, "compaction_in_progress", "a compaction is already running for this table and partition")
			default:
				writeError(c, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
			}
			return
		}
		c.JSON(http.StatusCreated, compactionView(compaction))
	}
}

func handleGetCompaction(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			writeError(c, http.StatusNotFound, "compaction_not_found", "compaction not found")
			return
		}
		compaction, found, err := st.GetCompaction(id)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
			return
		}
		if !found {
			writeError(c, http.StatusNotFound, "compaction_not_found", "compaction not found")
			return
		}
		c.JSON(http.StatusOK, compactionView(compaction))
	}
}

// compactionView renders a task together with the snapshot it was created from.
func compactionView(c store.Compaction) gin.H {
	return gin.H{
		"id":               c.ID,
		"table_id":         c.TableID,
		"partition_key":    c.PartitionKey,
		"partition_value":  c.PartitionValue,
		"file_count":       c.FileCount,
		"target_file_size": c.TargetFileSize,
		"status":           c.Status,
	}
}
