package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func appConfigViews(items []model.AppConfigItem) []model.AppConfigItem {
	out := slices.Clone(items)
	for i := range out {
		if out[i].Secret || appSensitiveKey.MatchString(out[i].Key) || strings.HasPrefix(out[i].Value, crypto.EncPrefix) {
			out[i].Secret = true
			out[i].HasValue = out[i].HasValue || out[i].Value != ""
			out[i].Value = ""
		}
	}
	return out
}

func (h *Handler) protectAppConfig(items []model.AppConfigItem, encrypted bool) ([]model.AppConfigItem, error) {
	out := slices.Clone(items)
	for i := range out {
		item := &out[i]
		item.EffectiveValue = ""
		if !item.Secret && !appSensitiveKey.MatchString(item.Key) {
			continue
		}
		item.Secret = true
		if !encrypted || item.Value == "" {
			continue
		}
		key := h.cfg.GetAESKey()
		if strings.HasPrefix(item.Value, crypto.EncPrefix) {
			if _, err := crypto.DecryptAESGCM(item.Value, key); err != nil {
				return nil, fmt.Errorf("could not securely store app configuration %s", item.Key)
			}
		}
		value, err := crypto.EncryptAESGCM(item.Value, key)
		if err != nil {
			return nil, fmt.Errorf("could not securely store app configuration %s", item.Key)
		}
		item.Value = value
	}
	return out, nil
}

func (h *Handler) openAppConfig(items []model.AppConfigItem) ([]model.AppConfigItem, error) {
	out := slices.Clone(items)
	for i := range out {
		if !strings.HasPrefix(out[i].Value, crypto.EncPrefix) {
			continue
		}
		value, err := crypto.DecryptAESGCM(out[i].Value, h.cfg.GetAESKey())
		if err != nil {
			return nil, fmt.Errorf("could not open app configuration %s", out[i].Key)
		}
		out[i].Value = value
		out[i].Secret = true
	}
	return out, nil
}

func (h *Handler) runtimeAppEnvironment(resources []config.OnlineResourceConfig, ids []string, items []model.AppConfigItem) ([]model.AppConfigItem, error) {
	items, err := h.openAppConfig(items)
	if err != nil {
		return nil, err
	}
	return mergeAppEnvironment(resources, ids, items)
}

type confirmedConfigDeclaration struct {
	Key               string `json:"key"`
	Required          bool   `json:"required"`
	Secret            bool   `json:"secret"`
	PreserveOnUpgrade bool   `json:"preserve_on_upgrade"`
	ValuePlan         struct {
		Kind string `json:"kind"`
	} `json:"value_plan"`
}

type confirmedConfigContract struct {
	Format           int                   `json:"config_format"`
	BaseRevision     string                `json:"base_config_revision"`
	EffectiveConfigs []model.AppConfigItem `json:"effective_configs"`
	Contract         struct {
		Configs []confirmedConfigDeclaration `json:"configs"`
	} `json:"contract"`
}

func (p confirmedConfigContract) apply(items []model.AppConfigItem) error {
	seen := make(map[string]bool, len(items))
	for i := range items {
		item := &items[i]
		index := slices.IndexFunc(p.Contract.Configs, func(config confirmedConfigDeclaration) bool { return config.Key == item.Key })
		if index < 0 || seen[item.Key] {
			return fmt.Errorf("configuration %s is not uniquely declared in the confirmed contract", item.Key)
		}
		config := p.Contract.Configs[index]
		seen[item.Key] = true
		item.Required, item.Secret, item.PreserveOnUpgrade = config.Required, config.Secret || item.Secret, config.PreserveOnUpgrade
	}
	for _, config := range p.Contract.Configs {
		if config.Required && (config.ValuePlan.Kind == "" || !seen[config.Key]) {
			return fmt.Errorf("missing confirmed configuration or value source for %s", config.Key)
		}
	}
	return nil
}

func (h *Handler) verifyConfirmedConfigValues(plan confirmedConfigContract, items []model.AppConfigItem, resources []config.OnlineResourceConfig, ids []string) error {
	if plan.EffectiveConfigs == nil {
		return nil
	}
	effective, err := h.appConfigValues(resources, ids, items)
	if err != nil {
		return err
	}
	for _, next := range effective {
		idx := slices.IndexFunc(plan.EffectiveConfigs, func(item model.AppConfigItem) bool { return item.Key == next.Key })
		if idx < 0 {
			return fmt.Errorf("configuration %s is missing from the confirmed values", next.Key)
		}
		approved := plan.EffectiveConfigs[idx]
		value := next.Value
		if next.ResourceID != "" {
			value = next.EffectiveValue
		}
		if approved.ResourceID != next.ResourceID || resourceEnvKey(approved) != resourceEnvKey(next) || approved.Value != value {
			return fmt.Errorf("configuration %s changed after confirmation; review the deployment values again", next.Key)
		}
	}
	return nil
}

var connectionIdentityKey = regexp.MustCompile(`(?i)^(?:DATABASE_URL|DB_(?:HOST|PORT|NAME|DATABASE|SCHEMA|URL|DSN)|PG(?:HOST|PORT|DATABASE)|REDIS_(?:HOST|PORT|URL|DB|DATABASE|KEY_PREFIX|NAMESPACE))$`)

func dataResourceDigests(items []model.AppConfigItem, resources []config.OnlineResourceConfig) string {
	digests := make(map[string]string)
	for _, resource := range resources {
		switch strings.ToLower(resource.Type) {
		case "postgres", "postgresql", "redis", "mysql", "mariadb", "sqlserver", "mongodb":
		default:
			continue
		}
		if !slices.ContainsFunc(items, func(item model.AppConfigItem) bool { return item.ResourceID == resource.ID }) {
			continue
		}
		target := make(map[string]string)
		for k, v := range resource.EnvVars {
			if connectionIdentityKey.MatchString(k) {
				target[k] = v
			}
		}
		data, _ := json.Marshal(target)
		digests[resource.ID] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	data, _ := json.Marshal(digests)
	return string(data)
}

func checkDataResourceDigests(ver *model.AppDeploymentVersion, items []model.AppConfigItem, resources []config.OnlineResourceConfig) error {
	if ver.DataBindingDigestsJSON != "" && ver.DataBindingDigestsJSON != dataResourceDigests(items, resources) {
		return fmt.Errorf("a data resource target changed after confirmation; reconcile the resource before deployment")
	}
	return nil
}

func (h *Handler) protectExistingDataConfig(previousJSON string, items []model.AppConfigItem, resources []config.OnlineResourceConfig, migration bool) error {
	if previousJSON == "" {
		return nil
	}
	var previous []model.AppConfigItem
	if err := json.Unmarshal([]byte(previousJSON), &previous); err != nil {
		return fmt.Errorf("could not read previous data configuration")
	}
	previous, err := h.openAppConfig(previous)
	if err != nil {
		return err
	}
	next, err := h.openAppConfig(items)
	if err != nil {
		return err
	}
	for _, old := range previous {
		if old.Value == "" && old.ResourceID == "" {
			continue
		}
		dataConnection := connectionIdentityKey.MatchString(old.Key)
		postgres := false
		for _, res := range resources {
			if old.ResourceID == "" || old.ResourceID != res.ID {
				continue
			}
			postgres = res.Type == "postgres" || res.Type == "postgresql"
			dataConnection = dataConnection || postgres || res.Type == "redis" || res.Type == "mysql" || res.Type == "sqlserver" || res.Type == "mongodb" || res.Type == "mariadb"
		}
		if !old.PreserveOnUpgrade && (!dataConnection || migration && postgres) {
			continue
		}
		idx := slices.IndexFunc(next, func(item model.AppConfigItem) bool { return item.Key == old.Key })
		if idx < 0 || next[idx].Value != old.Value || next[idx].ResourceID != old.ResourceID || resourceEnvKey(next[idx]) != resourceEnvKey(old) {
			return fmt.Errorf("configuration %s protects existing data and cannot be changed without a supported migration", old.Key)
		}
		items[idx].PreserveOnUpgrade = items[idx].PreserveOnUpgrade || old.PreserveOnUpgrade
	}
	return nil
}

func confirmedSnapshotFilename(revision string) string {
	return "confirmed-deployment-" + revision + ".json"
}

func (h *Handler) archiveConfirmedConfig(ctx context.Context, store *deploy.ArtifactStore, ver *model.AppDeploymentVersion, prefix string, resources []config.OnlineResourceConfig) error {
	var configs []model.AppConfigItem
	var mounts []model.AppStorageMount
	if err := json.Unmarshal([]byte(ver.AppConfigJSON), &configs); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(ver.StorageMountsJSON), &mounts); err != nil {
		return err
	}
	var ids []string
	if ver.ResourceIDsJSON != "" {
		if err := json.Unmarshal([]byte(ver.ResourceIDsJSON), &ids); err != nil {
			return err
		}
	}
	configs, err := h.appConfigValues(resources, ids, configs)
	if err != nil {
		return err
	}
	for i := range configs {
		configs[i].Description = ""
		configs[i].HasValue = configs[i].HasValue || configs[i].ResourceID != ""
	}
	for i := range mounts {
		mounts[i].HostPath = ""
	}
	body, err := json.MarshalIndent(map[string]any{
		"format": 1, "slug": ver.AppSlug, "version": ver.Version, "config_revision": ver.ConfigRevision,
		"request_id": ver.RequestID, "confirmed_by": ver.CreatedBy, "port": ver.ContainerPort,
		"app_tar_sha256": ver.AppTarSHA256, "skill_zip_sha256": ver.SkillZipSHA256, "delivery_plan_sha256": ver.DeliveryPlanSHA256,
		"app_config": configs, "storage_mounts": mounts,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := store.PutObject(ctx, prefix+"/"+confirmedSnapshotFilename(ver.ConfigRevision), body, "application/json; charset=utf-8"); err != nil {
		return err
	}
	ver.DeploymentSnapshotSHA = fmt.Sprintf("%x", sha256.Sum256(body))
	return nil
}
