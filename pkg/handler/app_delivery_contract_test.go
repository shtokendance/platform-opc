package handler

import (
	"encoding/json"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
)

func TestReviewedDeliveryMountsAllowResourceChoiceOnly(t *testing.T) {
	plan := []byte(`{"contract":{"storage_mounts":[{"name":"assets","kind":"uploads","container_path":"/app/assets","resource_id":"first"}]}}`)
	mounts := []model.AppStorageMount{{Name: "assets", Kind: "uploads", ContainerPath: "/app/assets", ResourceID: "confirmed"}}
	if err := validateDeliveryMounts(plan, mounts); err != nil {
		t.Fatal(err)
	}
	mounts[0].Kind = "sqlite"
	if err := validateDeliveryMounts(plan, mounts); err == nil {
		t.Fatal("the confirmed mount cannot change the authored storage kind")
	}
	if err := validateDeliveryMounts(plan, nil); err == nil {
		t.Fatal("the confirmed mounts cannot omit a declared target")
	}
}

func TestReviewedDataBindingsUseDeclaredPolicy(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	previous := `[{"key":"DB_HOST","value":"unrelated"}, {"key":"CUSTOM_STORE","value":"","resource_id":"old","resource_key":"CONNECTION","preserve_on_upgrade":true},{"key":"CIPHER","value":"original","preserve_on_upgrade":true}]`
	next := []model.AppConfigItem{
		{Key: "DB_HOST", Value: "updated"},
		{Key: "CUSTOM_STORE", ResourceID: "old", ResourceKey: "CONNECTION"},
		{Key: "CIPHER", Value: "original"},
	}
	if err := h.protectDeclaredDataConfig(previous, next, deliveryDataPlan{Strategy: "reuse"}); err != nil {
		t.Fatal(err)
	}
	if !next[1].PreserveOnUpgrade || !next[2].PreserveOnUpgrade {
		t.Fatal("declared protections were dropped")
	}
	next[1].ResourceID = "unused"
	if err := h.protectDeclaredDataConfig(previous, next, deliveryDataPlan{Strategy: "reuse"}); err == nil {
		t.Fatal("the declared custom data source changed without a migration")
	}
	plan := deliveryDataPlan{Strategy: "postgres_copy", Source: "old", Target: "new"}
	if err := h.protectDeclaredDataConfig(previous, next, plan); err == nil {
		t.Fatal("the declared data source switched to a different target than the migration")
	}
	next[1].ResourceID = "new"
	if err := h.protectDeclaredDataConfig(previous, next, plan); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRehearsalNeedsCompletedReceipt(t *testing.T) {
	started := time.Now().UTC()
	for _, state := range []string{
		"", "{}", `{"status":"failed"}`, `{"status":"running","target":"fixture","backup_sha256":"uncommitted"}`,
	} {
		if completedMigrationRehearsal(state) {
			t.Fatalf("incomplete rehearsal accepted: %s", state)
		}
	}
	old, _ := json.Marshal(postgresRehearsalStatus{Status: "running", StartedAt: started.Add(-2 * deliveryRehearsalTimeout)})
	if state := currentRehearsalStatus(string(old)).(postgresRehearsalStatus); state.Status != "failed" {
		t.Fatalf("interrupted rehearsal should show a recoverable failure: %+v", state)
	}
	completed, _ := json.Marshal(postgresMigrationReceipt{Target: "fixture", BackupSHA: "recorded", CompletedAt: started})
	if state := currentRehearsalStatus(string(completed)).(postgresMigrationReceipt); state.Status != "succeeded" {
		t.Fatalf("existing completed rehearsal must remain readable: %+v", state)
	}
}
