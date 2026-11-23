package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func (h *Handler) deliveryPreview(ctx context.Context, attempt model.AppDeploymentVersion) ([]model.AppConfigItem, []deploy.CandidateService, []string, []string, error) {
	var configs []model.AppConfigItem
	var mounts []model.AppStorageMount
	if json.Unmarshal([]byte(attempt.AppConfigJSON), &configs) != nil {
		return nil, nil, nil, nil, fmt.Errorf("approved candidate configuration could not be read")
	}
	if attempt.StorageMountsJSON != "" && json.Unmarshal([]byte(attempt.StorageMountsJSON), &mounts) != nil {
		return nil, nil, nil, nil, fmt.Errorf("approved candidate mounts could not be read")
	}
	configs, err := h.openAppConfig(configs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	paths := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		paths = append(paths, mount.ContainerPath)
	}
	// Only resource types are read here. Formal connection values and volume
	// backends are never used to construct the candidate launch request.
	resourceTypes := make(map[string]string)
	for _, item := range configs {
		if item.ResourceID != "" {
			for _, resource := range h.loadEffectiveOnlineResources(ctx) {
				resourceTypes[resource.ID] = strings.ToLower(resource.Type)
			}
			break
		}
	}
	items, services, skipped, err := previewEnvironment(attempt, h.cfg.Auth.Key, configs, resourceTypes)
	return items, services, paths, skipped, err
}

func previewEnvironment(attempt model.AppDeploymentVersion, key string, configs []model.AppConfigItem, types map[string]string) ([]model.AppConfigItem, []deploy.CandidateService, []string, error) {
	redisAuth := make(map[string]bool)
	for _, item := range configs {
		if types[item.ResourceID] == "redis" && strings.EqualFold(resourceEnvKey(item), "REDIS_PASSWORD") {
			redisAuth[item.ResourceID] = true
		}
	}
	indexes := make(map[string]int)
	ports := map[int]bool{attempt.ContainerPort: true}
	var services []deploy.CandidateService
	var skipped []string
	env := make([]model.AppConfigItem, 0, len(configs))
	for _, item := range configs {
		if item.ResourceID == "" {
			env = append(env, item)
			continue
		}
		kind := types[item.ResourceID]
		if kind == "postgresql" {
			kind = "postgres"
		}
		index, exists := indexes[item.ResourceID]
		if !exists {
			service := deploy.CandidateService{Kind: kind}
			switch kind {
			case "postgres":
				service.Port = 5432
			case "redis":
				service.Port = 6379
			default:
				skipped = append(skipped, item.Key)
				continue
			}
			if kind == "postgres" || redisAuth[item.ResourceID] {
				if key == "" {
					return nil, nil, nil, fmt.Errorf("candidate resource credentials could not be generated")
				}
				service.Password = previewPassword(key, attempt.ID.String(), item.ResourceID)
			}
			if ports[service.Port] {
				service.Port += 10000
				for ports[service.Port] {
					service.Port++
				}
			}
			if _, supported := previewResourceValue(service, resourceEnvKey(item)); !supported {
				skipped = append(skipped, item.Key)
				continue
			}
			if len(services) == 8 {
				return nil, nil, nil, fmt.Errorf("isolated candidates support at most eight temporary services")
			}
			index = len(services)
			indexes[item.ResourceID] = index
			ports[service.Port] = true
			services = append(services, service)
		}
		service := services[index]
		value, supported := previewResourceValue(service, resourceEnvKey(item))
		if !supported {
			skipped = append(skipped, item.Key)
			continue
		}
		item.Value = value
		item.Secret = item.Secret || (service.Password != "" && strings.Contains(value, service.Password))
		env = append(env, item)
	}
	return env, services, skipped, nil
}

func previewPassword(key, attemptID, resourceID string) string {
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "opc-preview/%s/%s", attemptID, resourceID)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func (h *Handler) redactCandidateLogs(logs string, attempt model.AppDeploymentVersion, configs []model.AppConfigItem) string {
	for _, cfg := range configs {
		if h.cfg.Auth.Key != "" && cfg.ResourceID != "" {
			logs = strings.ReplaceAll(logs, previewPassword(h.cfg.Auth.Key, attempt.ID.String(), cfg.ResourceID), "[REDACTED]")
		}
	}
	return redactAppRuntimeLogs(logs, configs)
}

func previewResourceValue(service deploy.CandidateService, key string) (string, bool) {
	const host = "127.0.0.1"
	port := strconv.Itoa(service.Port)
	addr := net.JoinHostPort(host, port)
	key = strings.ToUpper(key)
	if service.Kind == "postgres" {
		const db = deploy.CandidateDatabaseName
		values := map[string]string{
			"DB_HOST": host, "PGHOST": host, "POSTGRES_HOST": host,
			"DB_PORT": port, "PGPORT": port, "POSTGRES_PORT": port,
			"DB_NAME": db, "PGDATABASE": db, "POSTGRES_DB": db,
			"DB_USER": db, "PGUSER": db, "POSTGRES_USER": db,
			"DB_PASSWORD": service.Password, "PGPASSWORD": service.Password, "POSTGRES_PASSWORD": service.Password,
			"PGSSLMODE": "disable", "DB_SSLMODE": "disable",
		}
		switch key {
		case "DATABASE_URL", "DB_URL", "POSTGRES_URL":
			return (&url.URL{Scheme: "postgresql", User: url.UserPassword(db, service.Password), Host: addr, Path: "/" + db, RawQuery: "sslmode=disable"}).String(), true
		case "JDBC_DATABASE_URL":
			return "jdbc:postgresql://" + addr + "/" + db + "?sslmode=disable", true
		}
		value, ok := values[key]
		return value, ok
	}
	values := map[string]string{
		"REDIS_HOST": host, "REDIS_PORT": port, "REDIS_ADDR": addr, "REDIS_PASSWORD": service.Password,
		"REDIS_USER": "default", "REDIS_USERNAME": "default", "REDIS_DB": "0", "REDIS_DATABASE": "0", "REDIS_SSL": "false",
	}
	if key == "REDIS_URL" || key == "REDIS_CONNECTION_STRING" {
		u := url.URL{Scheme: "redis", Host: addr, Path: "/0"}
		if service.Password != "" {
			u.User = url.UserPassword("", service.Password)
		}
		return u.String(), true
	}
	value, ok := values[key]
	return value, ok
}
