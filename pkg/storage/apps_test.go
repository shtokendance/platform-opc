package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"weknora-tokendance/pkg/model"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupAppTestDB(t *testing.T) *Storage {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&model.DeployedApp{}); err != nil {
		t.Fatalf("failed to auto migrate DeployedApp: %v", err)
	}

	return &Storage{db: db}
}

func TestStorage_AppsCRUD(t *testing.T) {
	s := setupAppTestDB(t)
	ctx := context.Background()

	// 1. CheckSlugAvailable on empty db
	avail, err := s.CheckSlugAvailable(ctx, "expense-helper")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !avail {
		t.Errorf("expected slug to be available")
	}

	// 2. Create app
	app := &model.DeployedApp{
		Name:            "报销助手",
		Slug:            "expense-helper",
		Description:     "内网报销小助手",
		AppURL:          "http://expense-helper.apps.opc.intra",
		ContainerPort:   8080,
		Status:          model.AppStatusBuilding,
		AppConfigJSON:   `[{"key":"PORT","value":"8080"}]`,
		ResourceIDsJSON: `["shared-pg-main"]`,
		SkillName:       "app-expense-helper",
		CreatedBy:       "user-001",
	}

	if err := s.CreateOrUpdateApp(ctx, app); err != nil {
		t.Fatalf("failed to create app: %v", err)
	}

	// Slug should no longer be available
	avail, err = s.CheckSlugAvailable(ctx, "expense-helper")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if avail {
		t.Errorf("expected slug to be taken")
	}

	// 3. GetAppBySlug
	fetched, err := s.GetAppBySlug(ctx, "expense-helper")
	if err != nil {
		t.Fatalf("failed to get app: %v", err)
	}
	if fetched.Name != "报销助手" || fetched.Status != model.AppStatusBuilding {
		t.Errorf("fetched app mismatch: %+v", fetched)
	}

	// 4. UpdateAppStatus
	err = s.UpdateAppStatus(ctx, "expense-helper", model.AppStatusRunning, "Build complete", "", "cat_12345", true)
	if err != nil {
		t.Fatalf("failed to update app status: %v", err)
	}

	fetched, err = s.GetAppBySlug(ctx, "expense-helper")
	if err != nil {
		t.Fatalf("failed to get app after status update: %v", err)
	}
	if fetched.Status != model.AppStatusRunning || !fetched.SkillSynced || fetched.CatalogSkillID != "cat_12345" {
		t.Errorf("expected running and synced, got %+v", fetched)
	}

	// 5. UpdateAppConfig
	err = s.UpdateAppConfig(ctx, "expense-helper", 9000, `[{"key":"PORT","value":"9000"}]`, `["shared-pg-main","shared-redis-main"]`)
	if err != nil {
		t.Fatalf("failed to update app config: %v", err)
	}

	fetched, err = s.GetAppBySlug(ctx, "expense-helper")
	if err != nil {
		t.Fatalf("failed to get app after config update: %v", err)
	}
	if fetched.ContainerPort != 9000 || fetched.ResourceIDsJSON != `["shared-pg-main","shared-redis-main"]` {
		t.Errorf("expected updated port 9000 and resources, got %+v", fetched)
	}

	// 6. ListApps
	apps, err := s.ListApps(ctx)
	if err != nil {
		t.Fatalf("failed to list apps: %v", err)
	}
	if len(apps) != 1 || apps[0].Slug != "expense-helper" {
		t.Errorf("expected 1 app in list, got %d", len(apps))
	}
}

func TestOnlySuccessfulDeploymentsGetVersionNumbers(t *testing.T) {
	s := setupAppTestDB(t)
	ctx := t.Context()
	const slug = "audit-app"

	start := func(want int) *model.AppDeploymentVersion {
		t.Helper()
		number, err := s.NextAppVersionNumber(ctx, slug)
		if err != nil || number != want {
			t.Fatalf("next version = %d, err = %v, want %d", number, err, want)
		}
		app := &model.DeployedApp{Slug: slug, Name: slug, Status: model.AppStatusBuilding}
		attempt := &model.AppDeploymentVersion{AppSlug: slug, ProposedVersion: number, Status: model.AppStatusBuilding}
		if err := s.SavePublishedAppAndVersion(ctx, app, attempt); err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	promote := func(attempt *model.AppDeploymentVersion, want int) {
		t.Helper()
		version, err := s.PromoteAppDeployment(ctx, attempt.ID, slug, 18, "image", "", "ready", false, func(_ *model.AppDeploymentVersion) error {
			return nil
		})
		if err != nil || version != want {
			t.Fatalf("promoted v%d, err = %v, want v%d", version, err, want)
		}
		app, err := s.GetAppBySlug(ctx, slug)
		if err != nil || app.CurrentVersion != want || app.Status != model.AppStatusRunning {
			t.Fatalf("wrong current release: %+v, %v", app, err)
		}
	}

	for i := 0; i < 3; i++ {
		attempt := start(1)
		if i == 2 {
			archiveErr := errors.New("archive unavailable")
			_, err := s.PromoteAppDeployment(ctx, attempt.ID, slug, 18, "image", "", "", false, func(_ *model.AppDeploymentVersion) error {
				return archiveErr
			})
			if !errors.Is(err, archiveErr) {
				t.Fatalf("deployment cannot become a release without its archive: %v", err)
			}
		}
		if err := s.FailAppDeployment(ctx, attempt.ID, "failed", "failed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetAppVersion(ctx, slug, 1); err == nil {
		t.Fatal("failed attempts created a formal v1")
	}
	app, err := s.GetAppBySlug(ctx, slug)
	if err != nil || app.CurrentVersion != 0 {
		t.Fatalf("failed attempts changed current release: %+v, %v", app, err)
	}
	promote(start(1), 1)

	upgrade := start(2)
	if err := s.FailAppDeployment(ctx, upgrade.ID, "failed", "failed"); err != nil {
		t.Fatal(err)
	}
	active, err := s.GetActiveAppVersion(ctx, slug)
	if err != nil || active.Version != 1 {
		t.Fatalf("failed upgrade changed active release: %+v, %v", active, err)
	}
	promote(start(2), 2)
	if err := s.ActivateAppVersion(ctx, active, "", "", false); err != nil {
		t.Fatal(err)
	}
	promote(start(3), 3)
	attempts, err := s.ListAppVersions(ctx, slug)
	if err != nil || len(attempts) != 7 {
		t.Fatalf("lost deployment records: %+v, %v", attempts, err)
	}
}
