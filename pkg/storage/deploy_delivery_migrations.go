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
		var count int64
		if err := tx.Table("deploy_schema_migrations").Where("version = ?", deliverySchemaVersion).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		if err := tx.AutoMigrate(&model.DeployedApp{}, &model.DeployResource{}, &model.DockerfileTemplate{}, &model.AppDeploymentVersion{}, &model.AppDeliveryRevision{}); err != nil {
			return fmt.Errorf("apply delivery schema %s: %w", deliverySchemaVersion, err)
		}
		if err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_request_unique
			ON app_deployment_versions (app_slug, request_id)
			WHERE delivery_step <> '' AND request_id <> ''`).Error; err != nil {
			return err
		}
		return tx.Exec("INSERT INTO deploy_schema_migrations (version) VALUES (?)", deliverySchemaVersion).Error
	})
}
