package storage

import (
	"fmt"

	"weknora-tokendance/pkg/model"

	"gorm.io/gorm"
)

const deliverySchemaVersion = "000001_delivery_revisions_and_execution"

func migrateDeployDeliverySchema(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		postgres := tx.Dialector.Name() == "postgres"
		if postgres {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(301129618)").Error; err != nil {
				return err
			}
		}
		if err := tx.Exec(`CREATE TABLE IF NOT EXISTS deploy_schema_migrations (
			version VARCHAR(64) PRIMARY KEY,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`).Error; err != nil {
			return err
		}
		steps := []struct {
			version string
			apply   func() error
		}{
			{deliverySchemaVersion, func() error {
				if err := tx.AutoMigrate(&model.DeployedApp{}, &model.DeployResource{}, &model.DockerfileTemplate{}, &model.AppDeploymentVersion{}, &model.AppDeliveryRevision{}); err != nil {
					return err
				}
				return tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_request_unique
					ON app_deployment_versions (app_slug, request_id)
					WHERE delivery_step <> '' AND request_id <> ''`).Error
			}},
			{"000002_delivery_attempt_and_runtime_management", func() error {
				if err := tx.AutoMigrate(&model.AppDeploymentVersion{}, &model.AppRuntimeOperation{}); err != nil {
					return err
				}
				return tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_app_runtime_pending
					ON app_runtime_operations (app_id)
					WHERE state IN ('pending','restarting','checking','restoring','unknown','recovery_unknown')`).Error
			}},
			{"000003_app_volume_backups", func() error {
				return tx.AutoMigrate(&model.AppVolumeBackup{})
			}},
		}
		for _, step := range steps {
			var count int64
			if err := tx.Table("deploy_schema_migrations").Where("version = ?", step.version).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				continue
			}
			if err := step.apply(); err != nil {
				return fmt.Errorf("apply delivery schema %s: %w", step.version, err)
			}
			if err := tx.Exec("INSERT INTO deploy_schema_migrations (version) VALUES (?)", step.version).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
