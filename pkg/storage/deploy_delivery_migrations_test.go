package storage

import (
	"testing"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
)

func TestDeliverySchemaAndPromotionKeepPreviousRelease(t *testing.T) {
	s := setupAppTestDB(t)
	ctx := t.Context()
	if err := migrateDeployDeliverySchema(s.DeployDB()); err != nil {
		t.Fatal(err)
	}
	if err := migrateDeployDeliverySchema(s.DeployDB()); err != nil {
		t.Fatalf("delivery schema migration is not idempotent: %v", err)
	}
	app := &model.DeployedApp{Slug: "reviewed-app", Name: "Reviewed app", CurrentVersion: 1}
	if err := s.CreateOrUpdateApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	active := model.AppDeploymentVersion{ID: uuid.New(), AppSlug: app.Slug, Version: 1, IsActive: true, ConfigRevision: "running", Status: model.AppStatusRunning}
	if err := s.DeployDB().Create(&active).Error; err != nil {
		t.Fatal(err)
	}
	attempt := model.AppDeploymentVersion{
		ID: uuid.New(), AppSlug: app.Slug, Status: model.AppStatusPromoting, RequestID: "reviewed-request",
		DeliveryRevision: uuid.New(), DeliveryStep: "runtime_checked",
	}
	if err := s.SavePublishedAppAndVersion(ctx, app, &attempt); err != nil {
		t.Fatal(err)
	}
	duplicate := attempt
	duplicate.RequestID, duplicate.ID, duplicate.Status = "other-request", uuid.New(), model.AppStatusBuilding
	if err := s.SavePublishedAppAndVersion(ctx, app, &duplicate); err == nil {
		t.Fatal("accepted two pending deliveries for one app")
	}
	archiveCalls := 0
	archive := func(*model.AppDeploymentVersion) error {
		archiveCalls++
		return nil
	}
	if _, err := s.PromoteAppDeployment(ctx, attempt.ID, app.Slug, 2, "candidate", "", "", false, archive, model.AppDeliveryBase{Version: 1, ConfigRevision: "stale"}); err == nil || archiveCalls != 0 {
		t.Fatalf("stale approval reached promotion: error=%v archive calls=%d", err, archiveCalls)
	}
	if _, err := s.PromoteAppDeployment(ctx, attempt.ID, app.Slug, 2, "candidate", "", "", false, archive, model.AppDeliveryBase{Version: 1, ConfigRevision: "running"}); err != nil || archiveCalls != 1 {
		t.Fatalf("matching approval did not promote: error=%v archive calls=%d", err, archiveCalls)
	}
	if err := s.DeployDB().First(&attempt, "id = ?", attempt.ID).Error; err != nil || attempt.Status != model.AppStatusRunning || attempt.DeliveryStep != "cleanup_preview" {
		t.Fatalf("promoted receipt cannot recover cleanup: receipt=%+v error=%v", attempt, err)
	}
}
