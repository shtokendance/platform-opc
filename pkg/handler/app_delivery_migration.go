package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

type deliveryDataPlan struct {
	Strategy  string   `json:"strategy"`
	Summary   string   `json:"summary"`
	Recovery  string   `json:"recovery"`
	Source    string   `json:"source_resource_id"`
	Rehearsal string   `json:"rehearsal_resource_id"`
	Target    string   `json:"target_resource_id"`
	Files     []string `json:"migration_files"`
}

func readDeliveryDataPlan(data []byte) (deliveryDataPlan, error) {
	var plan struct {
		BaseVersion int `json:"base_version"`
		Contract    struct {
			Data deliveryDataPlan `json:"data_plan"`
		} `json:"contract"`
	}
	err := json.Unmarshal(data, &plan)
	s := plan.Contract.Data
	if err != nil || strings.TrimSpace(s.Summary) == "" || strings.TrimSpace(s.Recovery) == "" {
		return s, fmt.Errorf("the delivery must declare a data plan and recovery procedure")
	}
	switch s.Strategy {
	case "initialize":
		if plan.BaseVersion != 0 {
			return s, fmt.Errorf("initialization is only valid for a new application")
		}
	case "reuse":
		if plan.BaseVersion <= 0 || len(s.Files) > 0 || s.Source != "" || s.Target != "" || s.Rehearsal != "" {
			return s, fmt.Errorf("reuse describes existing storage with no separate migration")
		}
	case "postgres_copy":
		if plan.BaseVersion <= 0 || s.Source == "" || s.Rehearsal == "" || s.Target == "" ||
			s.Source == s.Rehearsal || s.Source == s.Target || s.Rehearsal == s.Target || len(s.Files) > 20 {
			return s, fmt.Errorf("PostgreSQL migration requires distinct source, rehearsal and production target resources")
		}
		for _, file := range s.Files {
			if !validDeliveryFilePath(file) || !strings.HasSuffix(strings.ToLower(file), ".sql") {
				return s, fmt.Errorf("migration files must be SQL paths in the application archive")
			}
		}
	default:
		return s, fmt.Errorf("unsupported data plan strategy")
	}
	return s, nil
}

func isDedicatedDeliveryResource(slug, id string) bool {
	if id == "db-"+slug {
		return true
	}
	prefix := "appdb-" + slug + "--"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(id, prefix)
	// UUIDs are unambiguous even when application names contain double hyphens.
	if len(suffix) != 32 {
		return false
	}
	_, err := uuid.Parse(suffix)
	return err == nil
}

func (h *Handler) deliveryPostgresDSN(ctx context.Context, slug, id string) (string, error) {
	if !isDedicatedDeliveryResource(slug, id) {
		return "", fmt.Errorf("resource %s is outside this application's dedicated database scope", id)
	}
	resource, err := h.storage.GetDeployResourceByID(ctx, id)
	if err != nil || !resource.Enabled || (resource.Type != "postgres" && resource.Type != "postgresql") {
		return "", fmt.Errorf("dedicated PostgreSQL resource %s is unavailable", id)
	}
	dsn := resource.EnvVars["DATABASE_URL"]
	if dsn == "" {
		return "", fmt.Errorf("dedicated PostgreSQL resource %s has no connection configured", id)
	}
	return dsn, nil
}

func (h *Handler) validateDataTargets(ctx context.Context, slug string, data deliveryDataPlan, nextResources []string) error {
	active, activeErr := h.storage.GetActiveAppVersion(ctx, slug)
	if activeErr != nil && !errors.Is(activeErr, gorm.ErrRecordNotFound) {
		return activeErr
	}
	var previous []string
	if active != nil && json.Unmarshal([]byte(active.ResourceIDsJSON), &previous) != nil {
		return fmt.Errorf("could not read the current data bindings")
	}
	if target, err := h.unpublishedMigrationTarget(ctx, slug, active); err != nil {
		return err
	} else if target != "" {
		// A switched candidate may have accepted writes even though it never became a release.
		if (data.Strategy != "reuse" || !slices.Contains(nextResources, target)) &&
			(data.Strategy != "postgres_copy" || data.Source != target) {
			return fmt.Errorf("the latest incomplete cutover may have data in %s; recover from that database before switching", target)
		}
		previous = []string{target}
	}
	if data.Strategy != "postgres_copy" {
		return nil
	}
	if !slices.Contains(previous, data.Source) || slices.Contains(previous, data.Target) || slices.Contains(previous, data.Rehearsal) ||
		!slices.Contains(nextResources, data.Target) || slices.Contains(nextResources, data.Source) || slices.Contains(nextResources, data.Rehearsal) {
		return fmt.Errorf("migration must copy an existing application binding to a dedicated target bound to the new version")
	}
	for _, id := range []string{data.Source, data.Rehearsal, data.Target} {
		if _, err := h.deliveryPostgresDSN(ctx, slug, id); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) unpublishedMigrationTarget(ctx context.Context, slug string, active *model.AppDeploymentVersion) (string, error) {
	query := h.storage.DeployDB().WithContext(ctx).Where("app_slug = ? AND version = 0 AND status = ? AND migration_execution <> '' AND delivery_step <> ?", slug, model.AppStatusFailed, "restored_before_switch")
	if active != nil {
		query = query.Where("created_at > ?", active.CreatedAt)
	}
	var attempt model.AppDeploymentVersion
	if err := query.Order("created_at DESC").First(&attempt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("could not read the last data cutover")
	}
	var result postgresMigrationReceipt
	if err := json.Unmarshal([]byte(attempt.MigrationExecution), &result); err != nil || !isDedicatedDeliveryResource(slug, result.Target) {
		return "", fmt.Errorf("the last data cutover receipt is invalid")
	}
	return result.Target, nil
}

func (h *Handler) allowChangeAfterMigration(c *gin.Context, slug string) bool {
	ctx := c.Request.Context()
	active, err := h.storage.GetActiveAppVersion(ctx, slug)
	if err == nil || errors.Is(err, gorm.ErrRecordNotFound) {
		var target string
		target, err = h.unpublishedMigrationTarget(ctx, slug, active)
		if err == nil && target != "" {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("recover the incomplete data cutover using %s before changing this application", target)})
			return false
		}
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "application data cutover state could not be read"})
		return false
	}
	return true
}

type postgresMigrationReceipt struct {
	Status         string    `json:"status,omitempty"`
	Source         string    `json:"source"`
	Target         string    `json:"target"`
	BackupKey      string    `json:"backup_key"`
	BackupSHA      string    `json:"backup_sha256"`
	MigrationFiles []string  `json:"migration_files"`
	CompletedAt    time.Time `json:"completed_at"`
}

const deliveryRehearsalTimeout = 10 * time.Minute

type postgresRehearsalStatus struct {
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func completedMigrationRehearsal(data string) bool {
	var receipt postgresMigrationReceipt
	return json.Unmarshal([]byte(data), &receipt) == nil &&
		(receipt.Status == "" || receipt.Status == "succeeded") &&
		receipt.Target != "" && receipt.BackupSHA != "" && !receipt.CompletedAt.IsZero()
}

func postgresToolEnv(dsn string) ([]string, error) {
	conn, err := url.Parse(dsn)
	if err != nil || (conn.Scheme != "postgres" && conn.Scheme != "postgresql") || conn.User == nil ||
		conn.Hostname() == "" || strings.TrimPrefix(conn.Path, "/") == "" {
		return nil, fmt.Errorf("the database resource requires a PostgreSQL URL")
	}
	pwd, _ := conn.User.Password()
	mode := conn.Query().Get("sslmode")
	if mode == "" {
		mode = "prefer"
	}
	port := conn.Port()
	if port == "" {
		port = "5432"
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PG") {
			env = append(env, entry)
		}
	}
	return append(env, "PGHOST="+conn.Hostname(), "PGPORT="+port, "PGUSER="+conn.User.Username(),
		"PGPASSWORD="+pwd, "PGDATABASE="+strings.TrimPrefix(conn.Path, "/"), "PGSSLMODE="+mode, "PGCONNECT_TIMEOUT=5"), nil
}

func (h *Handler) copyDeliveryPostgres(ctx context.Context, attempt model.AppDeploymentVersion, plan deliveryDataPlan, phase, targetID string) (postgresMigrationReceipt, error) {
	result := postgresMigrationReceipt{Source: plan.Source, Target: targetID}
	srcDSN, err := h.deliveryPostgresDSN(ctx, attempt.AppSlug, plan.Source)
	if err != nil {
		return result, err
	}
	dstDSN, err := h.deliveryPostgresDSN(ctx, attempt.AppSlug, targetID)
	if err != nil {
		return result, err
	}
	source, err := pgx.Connect(ctx, srcDSN)
	if err != nil {
		return result, fmt.Errorf("could not connect to the source database")
	}
	defer source.Close(context.Background())
	target, err := pgx.Connect(ctx, dstDSN)
	if err != nil {
		return result, fmt.Errorf("could not connect to the empty destination database")
	}
	defer target.Close(context.Background())
	identity := func(conn *pgx.Conn) (string, error) {
		var address, name string
		var port int
		err := conn.QueryRow(ctx, "select inet_server_addr()::text, inet_server_port(), current_database()").Scan(&address, &port, &name)
		return net.JoinHostPort(address, fmt.Sprint(port)) + "/" + name, err
	}
	src, a := identity(source)
	dst, b := identity(target)
	if a != nil || b != nil || src == dst {
		return result, fmt.Errorf("source and target must be distinct PostgreSQL databases")
	}
	otherID := plan.Rehearsal
	if targetID == otherID {
		otherID = plan.Target
	}
	if otherID != "" {
		otherDSN, err := h.deliveryPostgresDSN(ctx, attempt.AppSlug, otherID)
		if err != nil {
			return result, err
		}
		other, err := pgx.Connect(ctx, otherDSN)
		if err != nil {
			return result, fmt.Errorf("could not verify the other PostgreSQL destination")
		}
		peer, err := identity(other)
		_ = other.Close(ctx)
		if err != nil || peer == src || peer == dst {
			return result, fmt.Errorf("source, rehearsal and production target must be distinct PostgreSQL databases")
		}
	}
	if !safeSQLIdentRe.MatchString(target.Config().Database) {
		return result, fmt.Errorf("native restore requires a standard dedicated target database name")
	}
	var empty, privileged bool
	err = target.QueryRow(ctx, `SELECT NOT EXISTS (
		SELECT 1 FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND left(n.nspname, 3) <> 'pg_'
	), rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication FROM pg_roles WHERE rolname = current_user`).Scan(&empty, &privileged)
	if err != nil || !empty || privileged {
		return result, fmt.Errorf("destination must be empty and use a dedicated non-administrative role")
	}
	if phase == "cutover" {
		var connected int
		if err := source.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()").Scan(&connected); err != nil || connected > 0 {
			return result, fmt.Errorf("source database still has active client connections after the application stopped")
		}
	}
	srcEnv, err := postgresToolEnv(srcDSN)
	if err != nil {
		return result, err
	}
	dstEnv, err := postgresToolEnv(dstDSN)
	if err != nil {
		return result, err
	}
	file, err := os.CreateTemp("", "opc-delivery-*.dump")
	if err != nil {
		return result, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	dump := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--no-privileges")
	dump.Env, dump.Stdout = srcEnv, &limitedMigrationDump{file: file, remaining: 256 << 20}
	if err := dump.Run(); err != nil {
		return result, fmt.Errorf("PostgreSQL backup failed; check the database connection, client version and backup size")
	}
	backup, err := os.ReadFile(file.Name())
	if err != nil || len(backup) == 0 {
		return result, fmt.Errorf("backup is empty or unreadable")
	}
	result.BackupKey = fmt.Sprintf("apps/%s/attempts/%s/data/%s.dump", attempt.AppSlug, attempt.ID, phase)
	result.BackupSHA = fmt.Sprintf("%x", sha256.Sum256(backup))
	store := h.resolveArtifactStore(ctx)
	if err := store.PutObject(ctx, result.BackupKey, backup, "application/octet-stream"); err != nil {
		return result, fmt.Errorf("could not archive PostgreSQL backup")
	}
	restore := exec.CommandContext(ctx, "pg_restore", "--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges",
		"--dbname="+target.Config().Database, file.Name())
	restore.Env = dstEnv
	if err := restore.Run(); err != nil {
		return result, fmt.Errorf("could not restore into the dedicated empty destination")
	}
	archive, _, err := store.GetObject(ctx, attempt.AppTarObjectKey)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(archive)) != attempt.AppTarSHA256 {
		return result, fmt.Errorf("migration archive digest does not match the approved revision")
	}
	if err := pgx.BeginFunc(ctx, target, func(tx pgx.Tx) error {
		for _, path := range plan.Files {
			statement, err := readDeliveryArchiveFile(archive, path)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, statement); err != nil || target.PgConn().TxStatus() != 'T' {
				return fmt.Errorf("transactional migration failed in isolated target at %s", path)
			}
		}
		return nil
	}); err != nil {
		return result, err
	}
	result.MigrationFiles = plan.Files
	result.CompletedAt = time.Now()
	return result, nil
}

type limitedMigrationDump struct {
	file      *os.File
	remaining int
}

func (w *limitedMigrationDump) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		return 0, fmt.Errorf("database dump exceeds the configured delivery limit")
	}
	n, err := w.file.Write(data)
	w.remaining -= n
	return n, err
}

func (h *Handler) archivedDataPlan(ctx context.Context, attempt model.AppDeploymentVersion) (deliveryDataPlan, error) {
	key := fmt.Sprintf("apps/%s/attempts/%s/delivery.json", attempt.AppSlug, attempt.ID)
	data, _, err := h.resolveArtifactStore(ctx).GetObject(ctx, key)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != attempt.DeliveryPlanSHA256 {
		return deliveryDataPlan{}, fmt.Errorf("archived data plan is unavailable")
	}
	return readDeliveryDataPlan(data)
}

func (h *Handler) RehearseAppDelivery(c *gin.Context) {
	slug := c.Param("slug")
	ctx := c.Request.Context()
	db := h.storage.DeployDB().WithContext(ctx)
	var attempt model.AppDeploymentVersion
	query := db.Where("app_slug = ? AND request_id = ?", slug, c.Param("request_id"))
	if !h.canAccessAppSlug(ctx, slug, c.GetString(appOwnerKey)) {
		query = query.Where("created_by = ?", c.GetString(appOwnerKey))
	}
	if err := query.First(&attempt).Error; err != nil || attempt.DeliveryRevision == uuid.Nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no archived delivery rehearsal"})
		return
	}
	if attempt.MigrationRehearsal != "" {
		c.JSON(http.StatusOK, currentRehearsalStatus(attempt.MigrationRehearsal))
		return
	}
	if c.Request.Method == http.MethodGet {
		c.JSON(http.StatusOK, gin.H{"status": "not_started"})
		return
	}
	if attempt.Status != model.AppStatusBuilt || attempt.DeliveryStep != "build_complete" {
		c.JSON(http.StatusConflict, gin.H{"error": "the candidate build is not awaiting a migration rehearsal"})
		return
	}
	plan, err := h.archivedDataPlan(ctx, attempt)
	if err != nil || plan.Strategy != "postgres_copy" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "delivery does not declare a PostgreSQL rehearsal"})
		return
	}
	pending, _ := json.Marshal(postgresRehearsalStatus{Status: "running", StartedAt: time.Now().UTC()})
	claim := db.Model(&attempt).Where("status = ? AND delivery_step = ? AND migration_rehearsal = ''",
		model.AppStatusBuilt, attempt.DeliveryStep).Update("migration_rehearsal", string(pending))
	if claim.Error != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "migration rehearsal could not be queued"})
		return
	}
	if claim.RowsAffected == 0 {
		if err := db.First(&attempt, "id = ?", attempt.ID).Error; err != nil || attempt.MigrationRehearsal == "" {
			c.JSON(http.StatusConflict, gin.H{"error": "delivery phase changed before the rehearsal started"})
			return
		}
		c.JSON(http.StatusOK, currentRehearsalStatus(attempt.MigrationRehearsal))
		return
	}
	go h.runDeliveryRehearsal(attempt, plan, string(pending))
	c.Data(http.StatusAccepted, "application/json", pending)
}

func currentRehearsalStatus(data string) any {
	if completedMigrationRehearsal(data) {
		var receipt postgresMigrationReceipt
		_ = json.Unmarshal([]byte(data), &receipt)
		receipt.Status = "succeeded"
		return receipt
	}
	var state postgresRehearsalStatus
	if json.Unmarshal([]byte(data), &state) != nil ||
		(state.Status != "failed" && (state.Status != "running" || state.StartedAt.IsZero() || time.Since(state.StartedAt) > deliveryRehearsalTimeout+time.Minute)) {
		state.Status = "failed"
		state.Error = "the rehearsal has no completion receipt; inspect the destination and submit a new delivery with a new empty rehearsal database"
	}
	return state
}

func (h *Handler) runDeliveryRehearsal(attempt model.AppDeploymentVersion, plan deliveryDataPlan, pending string) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryRehearsalTimeout)
	defer cancel()
	result, err := h.copyDeliveryPostgres(ctx, attempt, plan, "rehearsal", plan.Rehearsal)
	var receipt any = result
	if err != nil {
		receipt = postgresRehearsalStatus{Status: "failed", Error: err.Error() + "; verify the rehearsal destination before preparing a retry"}
	} else {
		result.Status = "succeeded"
		receipt = result
	}
	data, _ := json.Marshal(receipt)
	persist, cancelPersist := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelPersist()
	h.storage.DeployDB().WithContext(persist).Model(&attempt).
		Where("migration_rehearsal = ?", pending).Update("migration_rehearsal", string(data))
}
