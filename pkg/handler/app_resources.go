package handler

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
)

var appEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var appSensitiveKey = regexp.MustCompile(`(?i)password|passwd|secret|token|dsn|database_url|connection|credential|api_key|access_key`)
var appSensitiveAssignment = regexp.MustCompile(`(?i)(password|passwd|secret|token|dsn|api[_-]?key|access[_-]?key)[^\s:=]*\s*[:=]\s*\S+`)

// Describe stored overrides and runtime injection separately. A blank override does not
// mean a selected resource's existing credential has been erased.
func describeAppConfig(resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem) []model.AppConfigItem {
	sources := make(map[string]string)
	for _, id := range ids {
		for _, resource := range resources {
			if resource.ID == id {
				for key, value := range resource.EnvVars {
					if value != "" {
						sources[key] = resource.Name
						if sources[key] == "" {
							sources[key] = id
						}
					}
				}
			}
		}
	}
	out := make([]model.AppConfigItem, len(configs))
	copy(out, configs)
	for i := range out {
		if out[i].ResourceID != "" {
			out[i].HasValue = false
			out[i].ValueSource = "resource"
			for _, resource := range resources {
				if resource.ID == out[i].ResourceID {
					out[i].HasValue = resource.EnvVars[out[i].Key] != ""
					out[i].SourceLabel = resource.Name
				}
			}
			continue
		}
		if out[i].ValueSource == "manual" || out[i].ValueSource == "original" {
			out[i].HasValue = out[i].Value != ""
			continue
		}
		out[i].HasValue = out[i].Value != "" || sources[out[i].Key] != ""
		out[i].ValueSource = "missing"
		out[i].SourceLabel = ""
		if out[i].Value != "" {
			out[i].ValueSource = "configured"
		} else if sources[out[i].Key] != "" {
			out[i].ValueSource = "resource"
			out[i].SourceLabel = sources[out[i].Key]
		}
	}
	return out
}

func redactAppRuntimeLogs(logs string, env []model.AppConfigItem) string {
	for _, item := range env {
		if item.Value != "" && appSensitiveKey.MatchString(item.Key) {
			logs = strings.ReplaceAll(logs, item.Value, "[REDACTED]")
		}
	}
	lines := strings.Split(logs, "\n")
	for i, line := range lines {
		if appSensitiveAssignment.MatchString(line) {
			lines[i] = "[REDACTED sensitive log line]"
		}
	}
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' && r != '\r' {
			return -1
		}
		return r
	}, strings.Join(lines, "\n"))
}

func mergeAppEnvironment(resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem) ([]model.AppConfigItem, error) {
	available := make(map[string]config.OnlineResourceConfig)
	for _, resource := range resources {
		available[resource.ID] = resource
	}
	values := make(map[string]string)
	// New clients confirm a resource for each field. Keep legacy bulk bindings for old apps.
	explicit := false
	for _, item := range configs {
		explicit = explicit || item.ResourceID != "" || item.ValueSource == "manual" || item.ValueSource == "original"
	}
	seenResources := make(map[string]bool)
	for _, id := range ids {
		resource, ok := available[id]
		if !ok || seenResources[id] {
			return nil, fmt.Errorf("unknown or duplicate resource: %s", id)
		}
		seenResources[id] = true
		if explicit {
			continue
		}
		for key, value := range resource.EnvVars {
			if !appEnvName.MatchString(key) {
				return nil, fmt.Errorf("invalid resource environment key")
			}
			if _, exists := values[key]; exists {
				return nil, fmt.Errorf("selected resources have conflicting key: %s", key)
			}
			values[key] = value
		}
	}
	seenConfigs := make(map[string]bool)
	for _, item := range configs {
		if !appEnvName.MatchString(item.Key) || seenConfigs[item.Key] {
			return nil, fmt.Errorf("invalid or duplicate application key: %s", item.Key)
		}
		seenConfigs[item.Key] = true
		if explicit {
			if item.ResourceID != "" {
				resource, ok := available[item.ResourceID]
				if !ok || !seenResources[item.ResourceID] || resource.EnvVars[item.Key] == "" || item.Value != "" {
					return nil, fmt.Errorf("invalid resource binding for key: %s", item.Key)
				}
				values[item.Key] = resource.EnvVars[item.Key]
			} else {
				values[item.Key] = item.Value
			}
			if item.Required && values[item.Key] == "" {
				return nil, fmt.Errorf("missing required configuration: %s", item.Key)
			}
			continue
		}
		// Blank UI fields represent server-side resource injection, not an instruction to erase it.
		if item.Value != "" || values[item.Key] == "" {
			values[item.Key] = item.Value
		}
		if item.Required && values[item.Key] == "" {
			return nil, fmt.Errorf("missing required configuration: %s", item.Key)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]model.AppConfigItem, 0, len(keys))
	for _, key := range keys {
		out = append(out, model.AppConfigItem{Key: key, Value: values[key]})
	}
	return out, nil
}

func resourcePreview(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		lower := strings.ToLower(key + " " + value)
		sensitive := strings.Contains(value, "@") || strings.Contains(value, "-----BEGIN")
		for _, word := range []string{"password", "passwd", "secret", "token", "api_key", "access_key", "dsn", "connectionstring", "connection_string"} {
			sensitive = sensitive || strings.Contains(lower, word)
		}
		if sensitive {
			out[key] = "[REDACTED]"
		} else {
			out[key] = value
		}
	}
	return out
}

// appendStorageMountEnvVars 当应用配置了持久化存储挂载点时，自动注入存储元数据环境变量；
// 若绑定了 S3 / OSS 对象存储或平台启用了 OPC_STORAGE_DRIVER=s3|oss，则同步注入云存储连接凭证。
func appendStorageMountEnvVars(slug string, envList []model.AppConfigItem, mounts []model.AppStorageMount, resources []config.OnlineResourceConfig) []model.AppConfigItem {
	if len(mounts) == 0 {
		return envList
	}
	existing := make(map[string]string, len(envList))
	out := make([]model.AppConfigItem, 0, len(envList)+8)
	for _, item := range envList {
		existing[item.Key] = item.Value
		out = append(out, item)
	}

	setIfMissing := func(key, val string) {
		if val == "" {
			return
		}
		if _, ok := existing[key]; !ok {
			existing[key] = val
			out = append(out, model.AppConfigItem{Key: key, Value: val})
		}
	}

	paths := make([]string, 0, len(mounts))
	for _, m := range mounts {
		if m.ContainerPath != "" {
			paths = append(paths, m.ContainerPath)
		}
		// 为常见目录注入标准环境变量（如 DATA_DIR=/app/data, UPLOAD_DIR=/app/uploads），方便支持环境变量覆盖的应用直接读取
		switch m.Name {
		case "data":
			setIfMissing("DATA_DIR", m.ContainerPath)
		case "uploads", "upload":
			setIfMissing("UPLOAD_DIR", m.ContainerPath)
		case "reports":
			setIfMissing("REPORT_DIR", m.ContainerPath)
		}
	}

	setIfMissing("OPC_APP_SLUG", slug)
	setIfMissing("OPC_STORAGE_MOUNTS", strings.Join(paths, ","))
	setIfMissing("OPC_STORAGE_PREFIX", "app-data/"+slug)

	driver := strings.ToLower(strings.TrimSpace(os.Getenv("OPC_STORAGE_DRIVER")))
	hasS3Bound := existing["S3_ENDPOINT"] != "" || existing["OSS_ENDPOINT"] != ""
	if (driver == "s3" || driver == "oss") && !hasS3Bound {
		// 从平台默认 s3 / oss 资源自动补齐对象存储连接串
		for _, r := range resources {
			t := strings.ToLower(strings.TrimSpace(r.Type))
			if t == "s3" || t == "oss" || t == "minio" {
				for k, v := range r.EnvVars {
					setIfMissing(k, v)
				}
				hasS3Bound = existing["S3_ENDPOINT"] != "" || existing["OSS_ENDPOINT"] != ""
				if hasS3Bound {
					break
				}
			}
		}
	}
	if hasS3Bound && driver == "" {
		driver = "s3"
	}
	if driver == "" {
		driver = "local"
	}
	setIfMissing("OPC_STORAGE_DRIVER", driver)
	return out
}
