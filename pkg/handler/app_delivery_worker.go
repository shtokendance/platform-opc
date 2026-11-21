package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"weknora-tokendance/pkg/model"
)

const deliveryLeaseDuration = 45 * time.Second

// RecoverAppDeliveries claims work by its persisted receipt. The same worker
// handles new requests and reconciliation after a gateway restart.
func (h *Handler) RecoverAppDeliveries(ctx context.Context) {
	if h.caproverClient == nil {
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		var jobs []model.AppDeploymentVersion
		if err := h.storage.DeployDB().WithContext(ctx).
			Where("delivery_step <> '' AND (delivery_lease_until IS NULL OR delivery_lease_until < ?)", time.Now().UTC()).
			Where("status IN ? OR delivery_step = ?", []string{model.AppStatusBuilding, model.AppStatusActivating, model.AppStatusPromoting}, deliveryCleanup).
			Order("updated_at").Limit(8).Find(&jobs).Error; err != nil {
			log.Printf("[AppDelivery] could not read pending work: %v", err)
		}
		for _, job := range jobs {
			go h.executeAppDelivery(ctx, job)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) executeAppDelivery(parent context.Context, attempt model.AppDeploymentVersion) {
	if attempt.DeliveryStep == "" || parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	token := uuid.NewString()
	now := time.Now().UTC()
	row := h.storage.DeployDB().Model(&model.AppDeploymentVersion{}).Where("id = ?", attempt.ID)
	claim := row.WithContext(ctx).Where("delivery_step = ? AND status = ? AND (delivery_lease_until IS NULL OR delivery_lease_until < ?)", attempt.DeliveryStep, attempt.Status, now).
		Updates(map[string]any{"delivery_lease": token, "delivery_lease_until": now.Add(deliveryLeaseDuration)})
	if claim.Error != nil || claim.RowsAffected != 1 {
		return
	}
	owned := row.Where("delivery_lease = ?", token)
	defer func() {
		finish, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		owned.WithContext(finish).Updates(map[string]any{"delivery_lease": "", "delivery_lease_until": nil})
	}()
	go func() {
		ticker := time.NewTicker(deliveryLeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				result := owned.WithContext(ctx).Update("delivery_lease_until", now.UTC().Add(deliveryLeaseDuration))
				if result.Error != nil || result.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	write := func(values map[string]any) error {
		result := owned.WithContext(ctx).Where("delivery_step = ? AND status = ?", attempt.DeliveryStep, attempt.Status).Updates(values)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("delivery changed while its operation was running")
		}
		if step, found := values["delivery_step"]; found {
			attempt.DeliveryStep = step.(string)
		}
		if status, found := values["status"]; found {
			attempt.Status = status.(string)
		}
		if message, found := values["error_message"]; found {
			attempt.ErrorMessage = message.(string)
		}
		if buildLog, found := values["build_log"]; found {
			attempt.BuildLog = buildLog.(string)
		}
		return nil
	}
	fail := func(message string) {
		_ = write(map[string]any{"status": model.AppStatusFailed, "error_message": message, "delivery_step": deliveryCleanup})
	}
	client := h.caproverClient
	if client == nil {
		fail("deployment service is unavailable")
		return
	}
	switch attempt.DeliveryStep {
	case deliveryCleanup:
		if err := client.RemoveCandidate(ctx, attempt.AppSlug, attempt.ID.String()); err == nil {
			_ = write(map[string]any{"delivery_step": ""})
		}
	case deliveryBuildQueued:
		store := h.resolveArtifactStore(ctx)
		if store == nil {
			fail("approved archive storage is unavailable")
			return
		}
		archive, _, err := store.GetObject(ctx, attempt.AppTarObjectKey)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(archive)) != attempt.AppTarSHA256 {
			fail("approved source archive is unavailable or does not match its receipt")
			return
		}
		plan, err := h.readAttemptDelivery(ctx, attempt)
		if err != nil {
			fail(err.Error())
			return
		}
		var approvedConfigs []model.AppConfigItem
		if json.Unmarshal([]byte(attempt.AppConfigJSON), &approvedConfigs) != nil {
			fail("approved build configuration could not be read")
			return
		}
		buildArgs, err := resolveDeliveryBuildInputs(plan, approvedConfigs, attempt.DockerfileContent)
		if err != nil {
			fail(err.Error())
			return
		}
		if err := client.EnsureCandidateAppRegistered(ctx, attempt.AppSlug); err != nil {
			fail(redactAppRuntimeLogs(err.Error(), nil))
			return
		}
		if err := client.UploadCandidateTar(ctx, attempt.AppSlug, archive, attempt.ID.String(), buildArgs); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			// A detached request may already have reached Deploy. Read the
			// authoritative build receipt before deciding that the job failed.
			log.Printf("[AppDelivery][%s] candidate submission did not confirm: %v", attempt.ID, redactAppRuntimeLogs(err.Error(), nil))
		}
		image, logs, err := client.WaitCandidateImage(ctx, attempt.AppSlug, attempt.ID.String())
		if err != nil {
			if ctx.Err() == nil {
				_ = write(map[string]any{"status": model.AppStatusFailed, "build_log": redactAppRuntimeLogs(logs, nil), "error_message": redactAppRuntimeLogs(err.Error(), nil), "delivery_step": deliveryCleanup})
			}
			return
		}
		_ = write(map[string]any{
			"status": model.AppStatusBuilt, "delivery_step": "build_complete", "deployed_image_name": image,
			"build_log": "Candidate image built. The formal application was not changed.\n" + redactAppRuntimeLogs(logs, nil),
		})
	case deliveryPreviewQueued:
		var configs []model.AppConfigItem
		if json.Unmarshal([]byte(attempt.AppConfigJSON), &configs) != nil {
			fail("approved configuration could not be read")
			return
		}
		items, err := h.openAppConfig(configs)
		if err != nil {
			fail(err.Error())
			return
		}
		local := make([]model.AppConfigItem, 0, len(items))
		for _, item := range items {
			if item.ResourceID == "" {
				local = append(local, item)
			}
		}
		if err := client.StartCandidate(ctx, attempt.AppSlug, attempt.ID.String(), local); err != nil {
			fail(redactAppRuntimeLogs(err.Error(), local))
			return
		}
		_ = write(map[string]any{
			"status": model.AppStatusRuntimeReady, "delivery_step": deliveryPreviewRunning,
			"build_log": attempt.BuildLog + "\n[Candidate] Started in an isolated network namespace. No live mounts or resource bindings are attached. Obtain actual application evidence before approval.",
		})
	default:
		h.executeDeliveryCutover(ctx, &attempt, write)
	}
}

func (h *Handler) executeDeliveryCutover(ctx context.Context, attempt *model.AppDeploymentVersion, write func(map[string]any) error) {
	client := h.caproverClient
	slug := attempt.AppSlug
	fail := func(message string) {
		_ = write(map[string]any{"status": model.AppStatusFailed, "error_message": message, "delivery_step": deliveryCleanup})
	}
	stop := func(step, message string) {
		// Reconciliation will finish the recovery even if the request or process stops here.
		_ = write(map[string]any{"delivery_step": step, "error_message": message})
	}
	previous, previousErr := h.storage.GetActiveAppVersion(ctx, slug)
	if previousErr != nil && !errors.Is(previousErr, gorm.ErrRecordNotFound) {
		return
	}
	planBytes, err := h.readAttemptDelivery(ctx, *attempt)
	if err != nil && attempt.DeliveryStep == deliveryCutoverQueued {
		fail(err.Error())
		return
	}
	if attempt.DeliveryStep == deliveryRestorePrevious {
		if previous == nil || previous.DeployedImageName == "" {
			if err := client.SetApplicationWriteHold(ctx, slug, true); err != nil {
				return
			}
		} else if err := h.restoreDeliveryVersion(ctx, *previous); err != nil {
			return
		}
		_ = client.RemoveCandidate(ctx, slug, attempt.ID.String())
		_ = write(map[string]any{"status": model.AppStatusFailed, "delivery_step": "restored_before_switch"})
		return
	}
	if attempt.DeliveryStep == deliveryFenceNew {
		if err := client.SetApplicationWriteHold(ctx, slug, true); err != nil {
			return
		}
		fail(attempt.ErrorMessage)
		return
	}
	if attempt.DeliveryStep == deliveryWriteHold || attempt.DeliveryStep == deliveryCopyStarted {
		stop(deliveryRestorePrevious, "Previous service is being restored after an interrupted copy. The destination was preserved and will not be reused automatically.")
		return
	}
	plan, planErr := readDeliveryDataPlan(planBytes)
	base, baseErr := readDeliveryBase(planBytes)
	if err != nil || planErr != nil || baseErr != nil {
		if attempt.DeliveryStep == deliverySwitchRequested || attempt.DeliveryStep == deliveryRuntimeChecked {
			stop(deliveryFenceNew, "The approved archive is unavailable after switch request. Container writes must stay fenced until recovery.")
		} else {
			stop(deliveryRestorePrevious, "The approved archive is unavailable. Restoring the previous service.")
		}
		return
	}
	if err := h.storage.CheckAppDeliveryBase(ctx, slug, base); err != nil {
		if attempt.DeliveryStep == deliveryCutoverQueued {
			fail(err.Error())
		} else if attempt.DeliveryStep == deliverySwitchRequested || attempt.DeliveryStep == deliveryRuntimeChecked {
			stop(deliveryFenceNew, "The approved baseline changed after switch request. Container writes must stay fenced until recovery.")
		} else {
			stop(deliveryRestorePrevious, "The approved baseline changed. Restoring the previous service.")
		}
		return
	}
	var configs []model.AppConfigItem
	var ids []string
	var mounts []model.AppStorageMount
	if json.Unmarshal([]byte(attempt.AppConfigJSON), &configs) != nil || json.Unmarshal([]byte(attempt.ResourceIDsJSON), &ids) != nil || json.Unmarshal([]byte(attempt.StorageMountsJSON), &mounts) != nil {
		if attempt.DeliveryStep == deliverySwitchRequested || attempt.DeliveryStep == deliveryRuntimeChecked {
			stop(deliveryFenceNew, "The approved configuration could not be read after switch request.")
		} else if attempt.DeliveryStep == deliveryCutoverQueued {
			fail("approved configuration could not be read")
		} else {
			stop(deliveryRestorePrevious, "The approved configuration could not be read. Restoring the previous service.")
		}
		return
	}
	resources := h.loadEffectiveOnlineResources(ctx)
	if err := h.verifyArchivedDeliveryConfig(ctx, *attempt, configs, resources, ids); err != nil {
		if attempt.DeliveryStep == deliverySwitchRequested || attempt.DeliveryStep == deliveryRuntimeChecked {
			stop(deliveryFenceNew, err.Error())
		} else if attempt.DeliveryStep == deliveryCutoverQueued {
			fail(err.Error())
		} else {
			stop(deliveryRestorePrevious, err.Error())
		}
		return
	}
	if attempt.DeliveryStep == deliveryCutoverQueued {
		if err := h.validateDataTargets(ctx, slug, plan, ids); err != nil {
			fail(err.Error())
			return
		}
		if plan.Strategy == "postgres_copy" {
			if err := write(map[string]any{"delivery_step": deliveryWriteHold}); err != nil {
				return
			}
			if err := client.SetApplicationWriteHold(ctx, slug, true); err != nil {
				stop(deliveryRestorePrevious, "Application write hold could not be verified. Restoring the previous service.")
				return
			}
			if err := write(map[string]any{"delivery_step": deliveryCopyStarted}); err != nil {
				return
			}
			migration, err := h.copyDeliveryPostgres(ctx, *attempt, plan, "cutover", plan.Target)
			if err != nil {
				stop(deliveryRestorePrevious, err.Error())
				return
			}
			receipt, _ := json.Marshal(migration)
			if err := write(map[string]any{"migration_execution": string(receipt), "delivery_step": deliveryDataCopied}); err != nil {
				return
			}
			attempt.MigrationExecution = string(receipt)
			resources = h.loadEffectiveOnlineResources(ctx)
			if err := h.verifyArchivedDeliveryConfig(ctx, *attempt, configs, resources, ids); err != nil {
				stop(deliveryRestorePrevious, err.Error())
				return
			}
		}
	}
	if attempt.DeliveryStep == deliveryCutoverQueued || attempt.DeliveryStep == deliveryDataCopied {
		env, err := h.applicationEnvironment(ctx, slug, resources, ids, configs, true)
		if err == nil {
			err = client.PrestageAppConfigAndEnv(ctx, slug, attempt.ContainerPort, env, mounts)
		}
		if err != nil {
			if previous == nil && plan.Strategy != "postgres_copy" {
				fail(redactAppRuntimeLogs(err.Error(), env))
			} else {
				stop(deliveryRestorePrevious, redactAppRuntimeLogs(err.Error(), env))
			}
			return
		}
		if err := write(map[string]any{"delivery_step": deliveryConfigStaged}); err != nil {
			return
		}
	}
	if attempt.DeliveryStep == deliveryConfigStaged {
		if err := write(map[string]any{"delivery_step": deliverySwitchRequested}); err != nil {
			return
		}
		// Once this request is issued, the candidate might accept writes. A
		// subsequent retry checks the runtime and never blindly repeats a switch.
		if err := client.DeployExistingImage(ctx, slug, attempt.DeployedImageName); err != nil && ctx.Err() == nil {
			log.Printf("[AppDelivery][%s] formal switch not confirmed: %v", attempt.ID, redactAppRuntimeLogs(err.Error(), configs))
		}
	}
	if attempt.DeliveryStep == deliverySwitchRequested {
		runtimeLog, err := client.WaitDeployedRuntime(ctx, slug, attempt.DeployedImageName)
		if err == nil {
			err = h.verifyAllocatedVolumes(ctx, slug, mounts)
		}
		if err != nil {
			message := redactAppRuntimeLogs(err.Error(), configs)
			// An unchanged data binding can use the prior application image.
			// A copied database may now hold live writes and must be kept.
			if previous != nil && plan.Strategy != "postgres_copy" {
				stop(deliveryRestorePrevious, message)
			} else {
				stop(deliveryFenceNew, message)
			}
			return
		}
		if err := write(map[string]any{"delivery_step": deliveryRuntimeChecked, "build_log": attempt.BuildLog + "\n" + redactAppRuntimeLogs(runtimeLog, configs)}); err != nil {
			return
		}
	}
	if attempt.DeliveryStep != deliveryRuntimeChecked {
		return
	}
	capVersion, image, err := client.GetAppImageInfo(ctx, slug)
	if err != nil {
		return
	}
	if image != attempt.DeployedImageName {
		stop(deliveryFenceNew, "The running image changed before formal registration. Verify the live data binding before recovery.")
		return
	}
	store := h.resolveArtifactStore(ctx)
	if store == nil {
		return
	}
	tar, _, tarErr := store.GetObject(ctx, attempt.AppTarObjectKey)
	zip, _, zipErr := store.GetObject(ctx, attempt.SkillZipObjectKey)
	if tarErr != nil || zipErr != nil || fmt.Sprintf("%x", sha256.Sum256(tar)) != attempt.AppTarSHA256 || fmt.Sprintf("%x", sha256.Sum256(zip)) != attempt.SkillZipSHA256 {
		return
	}
	logs := fmt.Sprintf("[Artifact] app.tar:%s\n[Artifact] skill.zip:%s\n[Artifact] delivery.json:%s\n%s", attempt.AppTarSHA256, attempt.SkillZipSHA256, attempt.DeliveryPlanSHA256, attempt.BuildLog)
	_, err = h.storage.PromoteAppDeployment(ctx, attempt.ID, slug, capVersion, image, "", logs, false, func(release *model.AppDeploymentVersion) error {
		return h.archivePublishArtifacts(ctx, store, release, fmt.Sprintf("apps/%s/v%d", slug, release.Version), resources, tar, zip, planBytes)
	}, base)
	if err == nil {
		_ = client.RemoveCandidate(ctx, slug, attempt.ID.String())
	}
}

func (h *Handler) restoreDeliveryVersion(ctx context.Context, previous model.AppDeploymentVersion) error {
	var configs []model.AppConfigItem
	var ids []string
	var mounts []model.AppStorageMount
	if json.Unmarshal([]byte(previous.AppConfigJSON), &configs) != nil || json.Unmarshal([]byte(previous.ResourceIDsJSON), &ids) != nil || json.Unmarshal([]byte(previous.StorageMountsJSON), &mounts) != nil {
		return fmt.Errorf("could not read the previous application configuration")
	}
	resources := h.loadEffectiveOnlineResources(ctx)
	if err := checkDataResourceDigests(&previous, configs, resources); err != nil {
		return err
	}
	env, err := h.applicationEnvironment(ctx, previous.AppSlug, resources, ids, configs, previous.DeliveryPlanSHA256 != "")
	if err != nil {
		return err
	}
	client := h.caproverClient
	if err := client.SetApplicationWriteHold(ctx, previous.AppSlug, true); err != nil {
		return err
	}
	if err := client.PrestageAppConfigAndEnv(ctx, previous.AppSlug, previous.ContainerPort, env, mounts); err != nil {
		return err
	}
	if err := client.DeployExistingImage(ctx, previous.AppSlug, previous.DeployedImageName); err != nil {
		return err
	}
	_, err = client.WaitDeployedRuntime(ctx, previous.AppSlug, previous.DeployedImageName)
	return err
}
