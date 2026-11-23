package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"weknora-tokendance/pkg/model"
)

// GetAppDatabaseSchema describes the application's dedicated PostgreSQL resource.
// Connection settings stay on Platform and the query never reads business rows.
func (h *Handler) GetAppDatabaseSchema(c *gin.Context) {
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	active, _ := h.storage.GetActiveAppVersion(ctx, slug)
	id := strings.TrimSpace(c.Query("resource_id"))
	version := 0
	var ids []string
	if active != nil {
		version = active.Version
		_ = json.Unmarshal([]byte(active.ResourceIDsJSON), &ids)
	}
	recoveryTarget, err := h.unpublishedMigrationTarget(ctx, slug, active)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not read the application database recovery state"})
		return
	}
	if recoveryTarget != "" {
		ids = append([]string{recoveryTarget}, ids...)
	}
	if id == "" {
		for _, candidate := range ids {
			if isDedicatedDeliveryResource(slug, candidate) {
				id = candidate
				break
			}
		}
	}
	allowed := slices.Contains(ids, id)
	if requestID := c.Query("request_id"); !allowed && requestID != "" {
		var attempt model.AppDeploymentVersion
		if err := h.storage.DeployDB().WithContext(ctx).Where("app_slug = ? AND request_id = ? AND created_by = ?", slug, requestID, c.GetString(appOwnerKey)).First(&attempt).Error; err == nil && attempt.DeliveryRevision != uuid.Nil {
			if plan, err := h.archivedDataPlan(ctx, attempt); err == nil {
				allowed = (id == plan.Rehearsal && completedMigrationRehearsal(attempt.MigrationRehearsal)) ||
					(id == plan.Target && attempt.MigrationExecution != "")
			}
		}
	}
	if !allowed {
		c.JSON(http.StatusNotFound, gin.H{"error": "no dedicated database is bound to this application"})
		return
	}
	dsn, err := h.deliveryPostgresDSN(ctx, slug, id)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not connect to the dedicated database"})
		return
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not begin read-only inspection"})
		return
	}
	defer tx.Rollback(ctx)
	if table := c.Query("table"); table != "" {
		schema := c.DefaultQuery("schema", "public")
		if !safeSQLIdentRe.MatchString(schema) || !safeSQLIdentRe.MatchString(table) ||
			schema == "information_schema" || strings.HasPrefix(schema, "pg_") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provide an application table and schema"})
			return
		}
		var count int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, table}.Sanitize()).Scan(&count); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "table could not be counted within the query deadline"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"resource_id": id, "recovery_target": recoveryTarget, "table": fmt.Sprintf("%s.%s", schema, table), "row_count": count})
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT table_schema, table_name, column_name, data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema NOT IN ('information_schema', 'pg_catalog')
		  AND left(table_schema, 3) <> 'pg_'
		ORDER BY table_schema, table_name, ordinal_position
		LIMIT 1001`)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not read the dedicated database schema"})
		return
	}
	type column struct {
		Schema   string `json:"schema"`
		Table    string `json:"table"`
		Name     string `json:"name"`
		DataType string `json:"data_type"`
		Nullable string `json:"nullable"`
	}
	columns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[column])
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not read the dedicated database schema"})
		return
	}
	truncated := len(columns) > 1000
	if truncated {
		columns = columns[:1000]
	}
	indexRows, err := tx.Query(ctx, `SELECT schemaname, tablename, indexname FROM pg_indexes
		WHERE schemaname <> 'information_schema' AND left(schemaname,3) <> 'pg_'
		ORDER BY schemaname, tablename, indexname LIMIT 1001`)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not read database indexes"})
		return
	}
	type index struct {
		Schema string `json:"schema"`
		Table  string `json:"table"`
		Name   string `json:"name"`
	}
	indexes, err := pgx.CollectRows(indexRows, pgx.RowToStructByPos[index])
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not read database indexes"})
		return
	}
	if len(indexes) > 1000 {
		indexes = indexes[:1000]
		truncated = true
	}
	c.JSON(http.StatusOK, gin.H{"resource_id": id, "recovery_target": recoveryTarget, "version": version, "columns": columns, "indexes": indexes, "truncated": truncated})
}
