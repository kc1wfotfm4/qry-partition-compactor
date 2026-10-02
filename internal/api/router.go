package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

// NewRouter wires the public HTTP surface described in README.md. Every entry keeps the
// published error shape: a single top-level error object with string code and message.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	router.POST("/api/v1/tables/:id/partitions", handleUpsertPartition(st))
	router.GET("/api/v1/tables/:id/advice", handleListAdvice(st))
	router.POST("/api/v1/compactions", handleCreateCompaction(st))
	router.GET("/api/v1/compactions/:id", handleGetCompaction(st))

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}

// writeError renders the single published error shape.
func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// decodeBody parses a JSON request body and rewrites decoder failures into messages that name
// the offending field without leaking SQL, statements, stacks, or paths.
func decodeBody(c *gin.Context, dst any) error {
	if err := json.NewDecoder(c.Request.Body).Decode(dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return fmt.Errorf("field %q has an invalid type", typeErr.Field)
		}
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return errors.New("request body must be valid JSON")
	}
	return nil
}
