package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *Handler) buildArchivedDelivery(ctx context.Context, attempt *model.AppDeploymentVersion, archive []byte) {
	fail := func(logs string, err error) {
		saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.storage.FailAppDeployment(saveCtx, attempt.ID, redactAppRuntimeLogs(logs, nil), redactAppRuntimeLogs(err.Error(), nil))
	}
	client := h.caproverClient
	if client == nil {
		fail("", fmt.Errorf("deployment service unavailable"))
		return
	}
	if err := client.EnsureAppRegistered(ctx, attempt.AppSlug); err != nil {
		fail("", err)
		return
	}
	if err := client.UploadCandidateTar(ctx, attempt.AppSlug, archive, attempt.ID.String()); err != nil {
		fail("", err)
		return
	}
	image, logs, err := client.WaitCandidateImage(ctx, attempt.AppSlug, attempt.ID.String())
	if err != nil {
		fail(logs, err)
		return
	}
	_ = h.storage.DeployDB().WithContext(ctx).Model(attempt).
		Where("status = ?", model.AppStatusBuilding).
		Updates(map[string]any{
			"status": model.AppStatusBuilt, "deployed_image_name": image,
			"build_log": "Candidate image built. Application activation has not started.\n" + redactAppRuntimeLogs(logs, nil),
		}).Error
}

type deliveryAssessment struct {
	Phase    string   `json:"phase"`
	Outcome  string   `json:"outcome"`
	Report   string   `json:"report"`
	Evidence []string `json:"evidence"`
}

func (h *Handler) activeDeliveryAttempt(ctx context.Context, slug string) (bool, error) {
	var count int64
	err := h.storage.DeployDB().WithContext(ctx).Model(&model.AppDeploymentVersion{}).
		Where("app_slug = ? AND version = 0 AND status IN ?", slug,
			[]string{model.AppStatusBuilt, model.AppStatusActivating, model.AppStatusRuntimeReady}).
		Count(&count).Error
	return count > 0, err
}

func (h *Handler) allowAppChange(c *gin.Context, slug string) bool {
	pending, err := h.activeDeliveryAttempt(c.Request.Context(), slug)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery state could not be read"})
		return false
	}
	if pending {
		c.JSON(http.StatusConflict, gin.H{"error": "complete the pending delivery assessment before modifying this application"})
		return false
	}
	return true
}

func (h *Handler) AssessAppDelivery(c *gin.Context) {
	ctx := c.Request.Context()
	slug := c.Param("slug")
	var input deliveryAssessment
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	if err := c.ShouldBindJSON(&input); err != nil ||
		(input.Phase != "build" && input.Phase != "runtime") ||
		(input.Outcome != "passed" && input.Outcome != "failed") ||
		strings.TrimSpace(input.Report) == "" || len(input.Report) > 8<<10 ||
		len(input.Evidence) == 0 || len(input.Evidence) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "assessment requires a phase, outcome, report and observed evidence"})
		return
	}
	if !h.tryLockDeploySlug(slug) {
		c.JSON(http.StatusConflict, gin.H{"error": "application deployment is busy"})
		return
	}
	defer h.unlockDeploySlug(slug)
	db := h.storage.DeployDB().WithContext(ctx)
	var attempt model.AppDeploymentVersion
	if err := db.Where("app_slug = ? AND request_id = ? AND created_by = ?", slug, c.Param("request_id"), c.GetString(appOwnerKey)).
		First(&attempt).Error; err != nil || attempt.DeliveryRevision == uuid.Nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "delivery attempt not found"})
		return
	}
	reports := make(map[string]deliveryAssessment)
	if attempt.DeliveryAssessment != "" {
		if err := json.Unmarshal([]byte(attempt.DeliveryAssessment), &reports); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "stored assessment is invalid"})
			return
		}
	}
	if attempt.Status == model.AppStatusRunning || attempt.Status == model.AppStatusFailed ||
		(input.Phase == "build" && (attempt.Status == model.AppStatusActivating || attempt.Status == model.AppStatusRuntimeReady)) {
		c.JSON(http.StatusOK, appPublishReceipt(attempt))
		return
	}
	if (input.Phase == "build" && attempt.Status != model.AppStatusBuilt) ||
		(input.Phase == "runtime" && attempt.Status != model.AppStatusRuntimeReady) {
		c.JSON(http.StatusConflict, gin.H{"error": "delivery phase changed"})
		return
	}
	reports[input.Phase] = input
	reportsJSON, _ := json.Marshal(reports)
	if input.Outcome == "failed" {
		result := db.Model(&attempt).Where("status = ?", attempt.Status).Updates(map[string]any{
			"status": model.AppStatusFailed, "delivery_assessment": string(reportsJSON), "error_message": input.Report,
		})
		if result.Error != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assessment could not be saved"})
			return
		}
		if result.RowsAffected == 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "delivery phase changed"})
			return
		}
		attempt.Status, attempt.ErrorMessage = model.AppStatusFailed, input.Report
		attempt.DeliveryAssessment = string(reportsJSON)
		if input.Phase == "runtime" {
			_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, attempt.BuildLog, input.Report, "", false)
		}
		c.JSON(http.StatusOK, appPublishReceipt(attempt))
		return
	}
	if input.Phase == "build" {
		plan, err := h.archivedDataPlan(ctx, attempt)
		if err != nil || (plan.Strategy == "postgres_copy" && attempt.MigrationRehearsal == "") {
			c.JSON(http.StatusConflict, gin.H{"error": "an archived data plan and successful rehearsal are required before activation"})
			return
		}
		result := db.Model(&attempt).Where("status = ?", model.AppStatusBuilt).Updates(map[string]any{
			"status": model.AppStatusActivating, "delivery_assessment": string(reportsJSON),
		})
		if result.Error != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "activation could not be queued"})
			return
		}
		if result.RowsAffected == 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "delivery phase changed"})
			return
		}
		attempt.Status = model.AppStatusActivating
		attempt.DeliveryAssessment = string(reportsJSON)
		go h.activateArchivedDelivery(attempt)
		c.JSON(http.StatusAccepted, appPublishReceipt(attempt))
		return
	}
	if h.caproverClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment service unavailable"})
		return
	}
	capVersion, image, err := h.caproverClient.GetAppImageInfo(ctx, slug)
	if err != nil || image != attempt.DeployedImageName {
		c.JSON(http.StatusConflict, gin.H{"error": "running image has changed"})
		return
	}
	store := h.resolveArtifactStore(ctx)
	prefix := fmt.Sprintf("apps/%s/attempts/%s/", slug, attempt.ID)
	tar, _, tarErr := store.GetObject(ctx, attempt.AppTarObjectKey)
	zip, _, zipErr := store.GetObject(ctx, attempt.SkillZipObjectKey)
	plan, _, planErr := store.GetObject(ctx, prefix+"delivery.json")
	if tarErr != nil || zipErr != nil || planErr != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "prepared artifacts are unavailable"})
		return
	}
	if fmt.Sprintf("%x", sha256.Sum256(tar)) != attempt.AppTarSHA256 ||
		fmt.Sprintf("%x", sha256.Sum256(zip)) != attempt.SkillZipSHA256 ||
		fmt.Sprintf("%x", sha256.Sum256(plan)) != attempt.DeliveryPlanSHA256 {
		c.JSON(http.StatusConflict, gin.H{"error": "active artifacts do not match the approved delivery"})
		return
	}
	if err := db.Model(&attempt).Update("delivery_assessment", string(reportsJSON)).Error; err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assessment could not be saved"})
		return
	}
	receipts := fmt.Sprintf("[Artifact] app.tar:%s\n[Artifact] skill.zip:%s\n[Artifact] delivery.json:%s\n", attempt.AppTarSHA256, attempt.SkillZipSHA256, attempt.DeliveryPlanSHA256)
	resources := h.loadEffectiveOnlineResources(ctx)
	number, err := h.storage.PromoteAppDeployment(ctx, attempt.ID, slug, capVersion, image, "", receipts+attempt.BuildLog, false, func(release *model.AppDeploymentVersion) error {
		return h.archivePublishArtifacts(ctx, store, release, fmt.Sprintf("apps/%s/v%d", slug, release.Version), resources, tar, zip, plan)
	})
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approved release could not be published"})
		return
	}
	attempt.Status, attempt.Version, attempt.IsActive = model.AppStatusRunning, number, true
	attempt.DeliveryAssessment = string(reportsJSON)
	c.JSON(http.StatusOK, appPublishReceipt(attempt))
}

func (h *Handler) activateArchivedDelivery(attempt model.AppDeploymentVersion) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var configs []model.AppConfigItem
	var resourceIDs []string
	var mounts []model.AppStorageMount
	write := func(status, log, message string) {
		saveCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = h.storage.DeployDB().WithContext(saveCtx).Model(&attempt).
			Where("status = ?", model.AppStatusActivating).Updates(map[string]any{
			"status": status, "build_log": strings.TrimSpace(attempt.BuildLog + "\n" + log), "error_message": message,
		}).Error
	}
	if json.Unmarshal([]byte(attempt.AppConfigJSON), &configs) != nil ||
		json.Unmarshal([]byte(attempt.ResourceIDsJSON), &resourceIDs) != nil ||
		json.Unmarshal([]byte(attempt.StorageMountsJSON), &mounts) != nil {
		write(model.AppStatusFailed, "", "approved configuration could not be read")
		return
	}
	client := h.caproverClient
	if client == nil {
		write(model.AppStatusFailed, "", "deployment service unavailable")
		return
	}
	plan, err := h.archivedDataPlan(ctx, attempt)
	if err != nil {
		write(model.AppStatusFailed, "", err.Error())
		return
	}
	if err := h.validateDataTargets(ctx, attempt.AppSlug, plan, resourceIDs); err != nil {
		write(model.AppStatusFailed, "", err.Error())
		return
	}
	resources := h.loadEffectiveOnlineResources(ctx)
	if err := checkDataResourceDigests(&attempt, configs, resources); err != nil {
		write(model.AppStatusFailed, "", err.Error())
		return
	}
	env, err := h.applicationEnvironment(ctx, attempt.AppSlug, resources, resourceIDs, configs, true)
	if err != nil {
		write(model.AppStatusFailed, "", err.Error())
		return
	}
	if plan.Strategy == "postgres_copy" {
		if err := client.SetApplicationWriteHold(ctx, attempt.AppSlug, true); err != nil {
			write(model.AppStatusFailed, "", "Application write hold could not be verified. Check the previous instance before proceeding.")
			return
		}
		resumePrevious := func(message string) {
			resumeCtx, release := context.WithTimeout(context.Background(), 20*time.Second)
			defer release()
			if err := client.SetApplicationWriteHold(resumeCtx, attempt.AppSlug, false); err != nil {
				message += ". The prior application could not be released from its write hold."
			}
			write(model.AppStatusFailed, "", message)
		}
		migration, err := h.copyDeliveryPostgres(ctx, attempt, plan, "cutover", plan.Target)
		if err != nil {
			resumePrevious(err.Error())
			return
		}
		result, _ := json.Marshal(migration)
		if err := h.storage.DeployDB().WithContext(ctx).Model(&attempt).Update("migration_execution", string(result)).Error; err != nil {
			resumePrevious("Migration completed on the destination but its receipt could not be saved. Both databases are preserved for recovery.")
			return
		}
	}
	err = client.PrestageAppConfigAndEnv(ctx, attempt.AppSlug, attempt.ContainerPort, env, mounts)
	if err == nil {
		_ = h.storage.UpdateAppStatus(ctx, attempt.AppSlug, model.AppStatusActivating, "", "", "", false)
		err = client.DeployExistingImage(ctx, attempt.AppSlug, attempt.DeployedImageName)
	}
	if err != nil {
		_ = h.storage.UpdateAppStatus(ctx, attempt.AppSlug, model.AppStatusFailed, "", redactAppRuntimeLogs(err.Error(), env), "", false)
		write(model.AppStatusFailed, "", redactAppRuntimeLogs(err.Error(), env))
		return
	}
	log, err := client.WaitDeployedRuntime(ctx, attempt.AppSlug, attempt.DeployedImageName)
	if err != nil {
		_ = h.storage.UpdateAppStatus(ctx, attempt.AppSlug, model.AppStatusFailed, "", redactAppRuntimeLogs(err.Error(), env), "", false)
		write(model.AppStatusFailed, redactAppRuntimeLogs(log, env), redactAppRuntimeLogs(err.Error(), env))
		return
	}
	if err := h.verifyAllocatedVolumes(ctx, attempt.AppSlug, mounts); err != nil {
		write(model.AppStatusFailed, redactAppRuntimeLogs(log, env), err.Error())
		return
	}
	write(model.AppStatusRuntimeReady, redactAppRuntimeLogs(log, env), "")
}
