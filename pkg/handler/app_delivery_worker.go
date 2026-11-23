package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
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
		h.pollAppRuntimeOperations(ctx)
		var jobs []model.AppDeploymentVersion
		if err := h.storage.DeployDB().WithContext(ctx).
			Where("delivery_step <> '' AND (delivery_lease_until IS NULL OR delivery_lease_until < ?)", time.Now().UTC()).
			Where("status IN ? OR delivery_step IN ?", []string{model.AppStatusBuilding, model.AppStatusActivating, model.AppStatusPromoting},
				[]string{deliveryCleanup, deliveryCancelRequested, deliveryRestoringCleanup}).
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
	heartbeat := owned.WithContext(ctx)
	if attempt.DeliveryStep != deliveryCancelRequested {
		heartbeat = heartbeat.Where("delivery_step <> ?", deliveryCancelRequested)
	}
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
				result := heartbeat.Session(&gorm.Session{}).Update("delivery_lease_until", now.UTC().Add(deliveryLeaseDuration))
				if result.Error != nil || result.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	write := func(values map[string]any) error {
		var started time.Time
		if step, ok := values["delivery_step"].(string); ok && step != attempt.DeliveryStep {
			started = time.Now().UTC()
			values["delivery_phase_started_at"] = started
		}
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
		if !started.IsZero() {
			attempt.DeliveryPhaseStartedAt = &started
		}
		if kind, ok := values["delivery_failure_kind"].(string); ok {
			attempt.DeliveryFailureKind = kind
		}
		return nil
	}
	fail := func(message string) {
		_ = write(map[string]any{"status": model.AppStatusFailed, "error_message": message, "delivery_step": deliveryCleanup,
			"delivery_failure_kind": "platform"})
	}
	client := h.caproverClient
	if client == nil {
		fail("deployment service is unavailable")
		return
	}
	switch attempt.DeliveryStep {
	case deliveryCleanup, deliveryCancelRequested, deliveryRestoringCleanup:
		h.finishDeploymentCleanup(ctx, attempt, write)
	case deliveryBuildQueued, deliveryDomainBuildQueued:
		if attempt.DeliveryBuildSubmitted {
			h.observeDeploymentBuild(ctx, attempt, write)
			return
		}
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
			if time.Since(attempt.CreatedAt) >= deploymentBuildLimit {
				fail("部署平台长时间不可用，正在结束本次任务")
			} else if attempt.DeliveryBuildState != "unknown" {
				_ = write(map[string]any{"delivery_build_state": "unknown"})
			}
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
		if err := write(map[string]any{"delivery_build_submitted": true}); err != nil {
			return
		}
		h.observeDeploymentBuild(ctx, attempt, write)
	case deliveryPreviewQueued:
		env, services, mounts, skipped, err := h.deliveryPreview(ctx, attempt)
		if err != nil {
			fail(err.Error())
			return
		}
		if err := client.StartCandidate(ctx, attempt.AppSlug, attempt.ID.String(), env, services, mounts); err != nil {
			fail(redactAppRuntimeLogs(err.Error(), env))
			return
		}
		log := fmt.Sprintf("%s\n[Candidate] Started offline with %d temporary services and %d temporary volumes. Formal resources and data are not attached.", attempt.BuildLog, len(services), len(mounts))
		if len(skipped) > 0 {
			log += " Candidate configuration without an isolated equivalent: " + strings.Join(skipped, ", ") + "."
		}
		_ = write(map[string]any{
			"status": model.AppStatusRuntimeReady, "delivery_step": deliveryPreviewRunning,
			"build_log": log,
		})
	default:
		h.executeDeliveryCutover(ctx, &attempt, write)
	}
}

func (h *Handler) observeDeploymentBuild(ctx context.Context, attempt model.AppDeploymentVersion, write func(map[string]any) error) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	build, err := h.caproverClient.GetCandidateBuild(readCtx, attempt.AppSlug, attempt.ID.String())
	if ctx.Err() != nil {
		return
	}
	values := map[string]any{}
	if build.Log != "" {
		log := redactAppRuntimeLogs(build.Log, nil)
		if log != attempt.BuildLog {
			values["build_log"] = log
		}
	}
	switch {
	case err == nil && build.Status == "built" && build.Image != "":
		status, step := model.AppStatusBuilt, "build_complete"
		if attempt.DeliveryStep == deliveryDomainBuildQueued {
			status, step = model.AppStatusPromoting, deliveryCutoverQueued
		}
		values["status"], values["delivery_step"], values["deployed_image_name"] = status, step, build.Image
		values["build_log"] = "Candidate image built. The formal application was not changed.\n" + redactAppRuntimeLogs(build.Log, nil)
		values["delivery_build_state"] = "built"
	case err == nil && build.Status == "failed":
		values["status"], values["delivery_step"], values["delivery_failure_kind"] = model.AppStatusFailed, deliveryCleanup, "build"
		values["error_message"] = redactAppRuntimeLogs(build.Error, nil)
	case time.Since(attempt.CreatedAt) >= deploymentBuildLimit:
		values["status"], values["delivery_step"], values["delivery_failure_kind"] = model.AppStatusFailed, deliveryCleanup, "platform"
		values["error_message"] = "本次构建超过等待上限，平台正在停止任务并清理临时资源"
	default:
		state := "unknown"
		if err == nil && (build.Status == "building" || build.Status == "queued") {
			state = build.Status
		}
		if state != attempt.DeliveryBuildState {
			values["delivery_build_state"] = state
		}
	}
	if len(values) > 0 {
		_ = write(values)
	}
}

func (h *Handler) finishDeploymentCleanup(ctx context.Context, attempt model.AppDeploymentVersion, write func(map[string]any) error) {
	client := h.caproverClient
	id := attempt.ID.String()
	if attempt.Version == 0 && attempt.DeliveryStep != deliveryRestoringCleanup && attempt.DeliveryBuildState != "built" {
		stopped, err := client.StopCandidateBuild(ctx, attempt.AppSlug, id)
		if err != nil || !stopped {
			return
		}
	}
	if err := client.RemoveCandidate(ctx, attempt.AppSlug, id); err != nil {
		return
	}
	values := map[string]any{"delivery_step": ""}
	switch attempt.DeliveryStep {
	case deliveryCancelRequested:
		values["status"] = model.AppStatusCancelled
	case deliveryRestoringCleanup:
		values["status"], values["delivery_step"] = model.AppStatusFailed, "restored_before_switch"
	default:
		if attempt.Status == model.AppStatusFailed && attempt.DeliveryFailureKind == "" {
			values["delivery_failure_kind"] = "platform"
		}
	}
	if attempt.Version == 0 {
		if err := h.storage.DeployDB().WithContext(ctx).Model(&model.DeployedApp{}).
			Where("slug = ? AND current_version = 0", attempt.AppSlug).Update("status", model.AppStatusFailed).Error; err != nil {
			return
		}
	}
	_ = write(values)
}

func (h *Handler) executeDeliveryCutover(ctx context.Context, attempt *model.AppDeploymentVersion, write func(map[string]any) error) {
	client := h.caproverClient
	slug := attempt.AppSlug
	fail := func(message string) {
		_ = write(map[string]any{"status": model.AppStatusFailed, "error_message": message, "delivery_step": deliveryCleanup,
			"delivery_failure_kind": "platform"})
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
			_ = write(map[string]any{"status": model.AppStatusFailed, "delivery_step": model.DeliveryRecoveryRequiredStep})
			return
		}
		if err := h.restoreDeliveryVersion(ctx, *previous); err != nil {
			return
		}
		if err := write(map[string]any{"delivery_step": deliveryRestoringCleanup}); err == nil {
			h.finishDeploymentCleanup(ctx, *attempt, write)
		}
		return
	}
	if attempt.DeliveryStep == deliveryFenceNew {
		if err := client.SetApplicationWriteHold(ctx, slug, true); err != nil {
			return
		}
		if attempt.MigrationExecution != "" || attempt.DeliveryFailureKind == "" {
			_ = write(map[string]any{"status": model.AppStatusFailed, "delivery_step": model.DeliveryRecoveryRequiredStep})
		} else {
			_ = write(map[string]any{"status": model.AppStatusFailed, "delivery_step": deliveryCleanup})
		}
		return
	}
	if attempt.DeliveryStep == deliveryWriteHold || attempt.DeliveryStep == deliveryCopyStarted {
		stop(deliveryRestorePrevious, "Previous service is being restored after an interrupted copy. The destination was preserved and will not be reused automatically.")
		return
	}
	plan, planErr := readDeliveryDataPlan(planBytes)
	base, baseErr := readDeliveryBase(planBytes)
	hasRequestedSwitch := attempt.DeliveryStep == deliverySwitchRequested || attempt.DeliveryStep == deliveryCheckingDomain || attempt.DeliveryStep == deliveryRuntimeChecked
	if err != nil || planErr != nil || baseErr != nil {
		if hasRequestedSwitch {
			stop(deliveryFenceNew, "The approved archive is unavailable after switch request. Container writes must stay fenced until recovery.")
		} else {
			stop(deliveryRestorePrevious, "The approved archive is unavailable. Restoring the previous service.")
		}
		return
	}
	if err := h.storage.CheckAppDeliveryBase(ctx, slug, base); err != nil {
		if attempt.DeliveryStep == deliveryCutoverQueued {
			fail(err.Error())
		} else if hasRequestedSwitch {
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
		if hasRequestedSwitch {
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
		if hasRequestedSwitch {
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
		if plan.Strategy == "postgres_copy" && !completedMigrationRehearsal(attempt.MigrationRehearsal) {
			if attempt.MigrationRehearsal == "" {
				pending, _ := json.Marshal(postgresRehearsalStatus{Status: "running", StartedAt: time.Now().UTC()})
				if err := write(map[string]any{"migration_rehearsal": string(pending)}); err == nil {
					h.runDeliveryRehearsal(*attempt, plan, string(pending))
				}
			} else if state, ok := currentRehearsalStatus(attempt.MigrationRehearsal).(postgresRehearsalStatus); ok && state.Status == "failed" {
				fail(state.Error)
			}
			return
		}
		var previousMounts []model.AppStorageMount
		if previous != nil && previous.StorageMountsJSON != "" && json.Unmarshal([]byte(previous.StorageMountsJSON), &previousMounts) != nil {
			fail("the active version's storage bindings could not be read")
			return
		}
		if err := client.CreateVolumePaths(ctx, mounts, previousMounts); err != nil {
			fail(redactAppRuntimeLogs(err.Error(), configs))
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
			detail := redactAppRuntimeLogs(err.Error(), configs)
			log.Printf("[AppDelivery][%s] formal switch not confirmed: %s", attempt.ID, detail)
			if err := write(map[string]any{"build_log": attempt.BuildLog + "\n[Deploy] " + detail}); err != nil {
				return
			}
		}
	}
	if attempt.DeliveryStep == deliverySwitchRequested || attempt.DeliveryStep == deliveryCheckingDomain {
		app, err := h.storage.GetAppBySlug(ctx, slug)
		if err != nil {
			return
		}
		failureKind := "startup"
		var runtimeLog, domainLog string
		if attempt.DeliveryStep == deliverySwitchRequested {
			runtimeLog, err = client.WaitDeployedRuntime(ctx, slug, attempt.DeployedImageName)
			if err == nil {
				err = h.verifyAllocatedVolumes(ctx, slug, mounts)
				if err != nil {
					failureKind = "platform"
				}
			}
			if err == nil {
				if err := write(map[string]any{"delivery_step": deliveryCheckingDomain, "delivery_build_state": "built",
					"build_log": attempt.BuildLog + "\n" + runtimeLog}); err != nil {
					return
				}
			}
		}
		if err == nil {
			failureKind = "domain"
			domainLog, err = client.WaitDeployedAppDomain(ctx, slug, attempt.DeployedImageName, app.AppURL)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			running, detail, observed := client.ObserveDeployedRuntime(ctx, slug, attempt.DeployedImageName)
			if !observed {
				_ = write(map[string]any{"delivery_build_state": "unknown"})
				return
			}
			if running && failureKind == "startup" {
				if err := h.verifyAllocatedVolumes(ctx, slug, mounts); err == nil {
					_ = write(map[string]any{"delivery_step": deliveryCheckingDomain, "delivery_build_state": "built",
						"build_log": attempt.BuildLog + "\n[Runtime] " + detail})
					return
				}
			}
			env, envErr := h.runtimeAppEnvironment(resources, ids, configs)
			message := "Application startup or domain check failed"
			logs := attempt.BuildLog
			if envErr == nil {
				message = redactAppRuntimeLogs(err.Error(), env)
				logs += "\n[Deployment]\n" + redactAppRuntimeLogs(strings.Join([]string{runtimeLog, domainLog, message}, "\n"), env)
				logCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, image, imageErr := client.GetAppImageInfo(logCtx, slug)
				if imageErr == nil && image == attempt.DeployedImageName {
					if output, logErr := client.RuntimeLogs(logCtx, slug); logErr == nil && output != "" {
						logs += "\n[Container logs]\n" + redactAppRuntimeLogs(output, env)
					} else if logErr != nil {
						logs += "\n[Container logs] " + redactAppRuntimeLogs(logErr.Error(), env)
					}
				}
				cancel()
			}
			// An unchanged data binding can use the prior application image.
			// A copied database may now hold live writes and must be kept.
			step := deliveryFenceNew
			if previous != nil && plan.Strategy != "postgres_copy" {
				step = deliveryRestorePrevious
			}
			_ = write(map[string]any{"delivery_step": step, "error_message": message, "build_log": logs, "delivery_failure_kind": failureKind})
			return
		}
		if err := write(map[string]any{"delivery_step": deliveryRuntimeChecked, "build_log": attempt.BuildLog + "\n" + domainLog}); err != nil {
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
