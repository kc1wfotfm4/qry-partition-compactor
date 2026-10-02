package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

// NewRouter wires the public HTTP surface. The service contract in README.md describes the
// error shape every entry must keep.
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

	router.POST("/api/v1/tables/:id/partitions", upsertPartitionHandler(st))
	router.GET("/api/v1/tables/:id/advice", adviceHandler(st))
	router.POST("/api/v1/compactions", createCompactionHandler(st))
	router.GET("/api/v1/compactions/:id", getCompactionHandler(st))

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}

// writeError keeps every failure on the published single-error shape.
func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
