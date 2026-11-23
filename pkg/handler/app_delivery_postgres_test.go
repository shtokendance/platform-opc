package handler

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestFailedDeliveryPreservesNewDataBinding(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	ctx := t.Context()
	const slug = "failed-cutover"
	const source = "db-" + slug
	const target = "appdb-" + slug + "--00000000000000000000000000000001"
	now := time.Now()
	db := h.storage.DeployDB()
	if err := db.AutoMigrate(&model.AppDeploymentVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AppDeploymentVersion{
		ID: uuid.New(), AppSlug: slug, Version: 1, IsActive: true, Status: model.AppStatusRunning,
		ResourceIDsJSON: fmt.Sprintf(`[%q]`, source), CreatedAt: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AppDeploymentVersion{
		ID: uuid.New(), AppSlug: slug, Status: model.AppStatusFailed,
		MigrationExecution: fmt.Sprintf(`{"target":%q}`, target), CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	plan := deliveryDataPlan{Strategy: "reuse"}
	if err := h.validateDataTargets(ctx, slug, plan, []string{source}); err == nil || !strings.Contains(err.Error(), target) {
		t.Fatalf("expected the previous source to be rejected in favor of %s, got %v", target, err)
	}
	if err := h.validateDataTargets(ctx, slug, plan, []string{target}); err != nil {
		t.Fatalf("the repair must be able to use the failed candidate's data: %v", err)
	}
}

func TestDeliveryPostgresCopiesLatestDataWithoutModifyingSource(t *testing.T) {
	port := os.Getenv("OPC_TEST_POSTGRES_PORT")
	if port == "" {
		t.Skip("requires the dedicated local PostgreSQL fixture and native backup clients")
	}
	h, _ := setupTestAppHandler(t)
	const slug = "history-fixture"
	ctx := t.Context()
	dsn := func(db string) string {
		return "postgres://delivery_fixture:fixture_password@127.0.0.1:" + port + "/" + db + "?sslmode=disable"
	}
	plan := deliveryDataPlan{Strategy: "postgres_copy", Source: "db-" + slug,
		Rehearsal: "appdb-" + slug + "--" + fmt.Sprintf("%032x", 1),
		Target:    "appdb-" + slug + "--" + fmt.Sprintf("%032x", 2), Files: []string{"migration.sql"}}
	for id, name := range map[string]string{plan.Source: "fixture_source", plan.Rehearsal: "fixture_rehearsal", plan.Target: "fixture_target"} {
		if err := h.storage.UpsertDeployResource(ctx, &model.DeployResource{ID: id, Name: id, Type: "postgres", Enabled: true,
			EnvVars: map[string]string{"DATABASE_URL": dsn(name)}}); err != nil {
			t.Fatal(err)
		}
	}
	execSQL := func(db, sql string) {
		t.Helper()
		conn, err := pgx.Connect(ctx, dsn(db))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	execSQL("fixture_source", "CREATE TABLE orders (id integer PRIMARY KEY, amount int); INSERT INTO orders VALUES (1, 120)")
	payload := []byte("ALTER TABLE orders ADD COLUMN currency text NOT NULL DEFAULT 'CNY';")
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: plan.Files[0], Size: int64(len(payload)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	attempt := model.AppDeploymentVersion{ID: uuid.New(), AppSlug: slug, AppTarObjectKey: "fixture/app.tar", AppTarSHA256: fmt.Sprintf("%x", sha256.Sum256(archive.Bytes()))}
	if err := h.resolveArtifactStore(ctx).PutObject(ctx, attempt.AppTarObjectKey, archive.Bytes(), "application/x-tar"); err != nil {
		t.Fatal(err)
	}
	setTarget := func(database string) {
		t.Helper()
		if err := h.storage.UpsertDeployResource(ctx, &model.DeployResource{
			ID: plan.Target, Name: plan.Target, Type: "postgres", Enabled: true,
			EnvVars: map[string]string{"DATABASE_URL": dsn(database)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setTarget("fixture_rehearsal")
	if _, err := h.copyDeliveryPostgres(ctx, attempt, plan, "rehearsal", plan.Rehearsal); err == nil || !strings.Contains(err.Error(), "distinct PostgreSQL databases") {
		t.Fatalf("resource aliases can make rehearsal write to the production target: %v", err)
	}
	setTarget("fixture_target")
	if _, err := h.copyDeliveryPostgres(ctx, attempt, plan, "rehearsal", plan.Rehearsal); err != nil {
		t.Fatal(err)
	}
	execSQL("fixture_source", "INSERT INTO orders VALUES (2, 130)")
	receipt, err := h.copyDeliveryPostgres(ctx, attempt, plan, "cutover", plan.Target)
	if err != nil || receipt.BackupSHA == "" {
		t.Fatalf("migration: %v, receipt: %+v", err, receipt)
	}
	verify := func(db string, expectedRows int, expectedCurrency bool) {
		t.Helper()
		conn, err := pgx.Connect(ctx, dsn(db))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var rows int
		var currency bool
		err = conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders),
			EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='orders' AND column_name='currency')`).Scan(&rows, &currency)
		if err != nil || rows != expectedRows || currency != expectedCurrency {
			t.Fatalf("%s: rows=%d currency=%v error=%v", db, rows, currency, err)
		}
	}
	verify("fixture_source", 2, false)
	verify("fixture_rehearsal", 1, true)
	verify("fixture_target", 2, true)
	if _, err := h.copyDeliveryPostgres(ctx, attempt, plan, "cutover", plan.Target); err == nil {
		t.Fatal("a populated database was accepted for a retry")
	}
}
