package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"weknora-tokendance/pkg/model"
)

type CapRoverClient struct {
	baseURL         string
	password        string
	rootDomain      string
	httpClient      *http.Client
	PollInterval    time.Duration
	ProbeInterval   time.Duration
	StartupTimeout  time.Duration
	AppReadyTimeout time.Duration

	mu        sync.RWMutex
	authToken string
}

func NewCapRoverClient(baseURL, password, rootDomain string) *CapRoverClient {
	baseURL = strings.TrimRight(baseURL, "/")
	return &CapRoverClient{
		baseURL:       baseURL,
		password:      password,
		rootDomain:    rootDomain,
		httpClient:    &http.Client{Timeout: 60 * time.Second},
		PollInterval:  2 * time.Second,
		ProbeInterval: 1 * time.Second,
	}
}

type caproverResponse struct {
	Status      int             `json:"status"`
	Description string          `json:"description"`
	Data        json.RawMessage `json:"data"`
}

var ErrAppBuildInProgress = errors.New("CapRover 上一轮构建仍在进行（1108）")

func (c *CapRoverClient) getToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	token := c.authToken
	c.mu.RUnlock()
	if token != "" {
		return token, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authToken != "" {
		return c.authToken, nil
	}

	payload, _ := json.Marshal(map[string]string{"password": c.password})
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("caprover login request failed: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode login response: %w", err)
	}
	if result.Status != 100 {
		return "", fmt.Errorf("caprover login failed (status %d): %s", result.Status, result.Description)
	}

	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return "", fmt.Errorf("parse token data: %w", err)
	}

	c.authToken = data.Token
	return c.authToken, nil
}

// EnsureAppRegistered 确保应用在 CapRover 注册
func (c *CapRoverClient) EnsureAppRegistered(ctx context.Context, slug string) error {
	return c.ensureAppRegistered(ctx, slug, "register")
}

func (c *CapRoverClient) EnsureCandidateAppRegistered(ctx context.Context, slug string) error {
	return c.ensureAppRegistered(ctx, slug, "register-candidate")
}

func (c *CapRoverClient) ensureAppRegistered(ctx context.Context, slug, endpoint string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"appName":           slug,
		"hasPersistentData": false,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/user/apps/appDefinitions/"+endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register app request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode register response: %w", err)
	}

	// 100 表示成功；若已存在 (description 包含 already exists)，也是合法的幂等结果
	if result.Status != 100 && !strings.Contains(strings.ToLower(result.Description), "already exists") {
		return fmt.Errorf("register app failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}

// UpdateAppConfigAndEnv 注入应用配置、线上资源环境变量与容器持久化卷挂载（强制内网 HTTP，关闭 SSL）
func (c *CapRoverClient) UpdateAppConfigAndEnv(ctx context.Context, slug string, port int, envVars []model.AppConfigItem, mounts ...[]model.AppStorageMount) error {
	return c.updateAppConfigAndEnv(ctx, slug, port, envVars, false, mounts...)
}

// PrestageAppConfigAndEnv 在构建/切换镜像前仅落库预置配置与挂载点，不立刻重启当前在线容器，待新版本或回滚镜像就绪后一次原子生效
func (c *CapRoverClient) PrestageAppConfigAndEnv(ctx context.Context, slug string, port int, envVars []model.AppConfigItem, mounts ...[]model.AppStorageMount) error {
	return c.updateAppConfigAndEnv(ctx, slug, port, envVars, true, mounts...)
}

func (c *CapRoverClient) updateAppConfigAndEnv(ctx context.Context, slug string, port int, envVars []model.AppConfigItem, skipServiceUpdate bool, mounts ...[]model.AppStorageMount) error {
	type envVar struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	envs := make([]envVar, 0, len(envVars))
	for _, item := range envVars {
		envs = append(envs, envVar{Key: item.Key, Value: item.Value})
	}

	var mountList []model.AppStorageMount
	if len(mounts) > 0 {
		mountList = mounts[0]
	}
	if err := c.prepareVolumePaths(ctx, mountList, nil); err != nil {
		return err
	}
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	overrideJSON := BuildServiceUpdateOverrideJSON(slug, mountList)

	body := map[string]interface{}{
		"appName":               slug,
		"instanceCount":         1,
		"containerHttpPort":     port,
		"notExposeAsWebApp":     false,
		"forceSsl":              false,
		"websocketSupport":      true,
		"serviceUpdateOverride": overrideJSON,
		"envVars":               envs,
	}
	if skipServiceUpdate {
		body["skipServiceUpdate"] = true
	}
	payload, _ := json.Marshal(body)

	err = c.postAppDefinition(ctx, slug, token, payload)
	if !errors.Is(err, ErrAppBuildInProgress) {
		return err
	}
	// The engine rejected the update before applying it. Wait for its build
	// lock and retry the same config before uploading or switching an image.
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("等待构建释放超时，请稍后重新发布原交付包：%w", err)
		case <-ticker.C:
			if waitCtx.Err() != nil {
				continue
			}
			nextErr := c.postAppDefinition(waitCtx, slug, token, payload)
			if nextErr == nil {
				return nil
			}
			if waitCtx.Err() == nil {
				if !errors.Is(nextErr, ErrAppBuildInProgress) {
					return nextErr
				}
				err = nextErr
			}
		}
	}
}

func (c *CapRoverClient) postAppDefinition(ctx context.Context, slug, token string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/user/apps/appDefinitions/update", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("update app definition request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode update response: %w", err)
	}
	if result.Status == 1108 && strings.HasPrefix(result.Description, "Build in-progress for "+slug+".") {
		return fmt.Errorf("%w: %s", ErrAppBuildInProgress, result.Description)
	}
	if result.Status != 100 {
		return fmt.Errorf("update app definition failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}

// UploadAppTar 上传应用源码包 app.tar 触发构建
func (c *CapRoverClient) UploadAppTar(ctx context.Context, slug string, tarBytes []byte) error {
	return c.uploadAppTar(ctx, slug, tarBytes, "", nil)
}

func (c *CapRoverClient) UploadCandidateTar(ctx context.Context, slug string, tarBytes []byte, receipt string, buildArgs map[string]string) error {
	return c.uploadAppTar(ctx, slug, tarBytes, receipt, buildArgs)
}

func (c *CapRoverClient) uploadAppTar(ctx context.Context, slug string, tarBytes []byte, receipt string, buildArgs map[string]string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("sourceFile", "app.tar")
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(tarBytes); err != nil {
		return fmt.Errorf("write tar bytes: %w", err)
	}
	if len(buildArgs) > 0 {
		encoded, err := json.Marshal(buildArgs)
		if err != nil {
			return err
		}
		if err := writer.WriteField("buildArgs", string(encoded)); err != nil {
			return fmt.Errorf("write candidate build arguments: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}

	uploadURL := fmt.Sprintf("%s/api/v2/user/apps/appData/%s/?detached=1", c.baseURL, slug)
	if receipt != "" {
		uploadURL = fmt.Sprintf("%s/api/v2/user/apps/appData/%s/builds/%s", c.baseURL, url.PathEscape(slug), url.PathEscape(receipt))
	}
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload app tar request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode upload response: %w", err)
	}
	// CapRover returns 100 (STATUS_OK) or 101 (STATUS_OK_DEPLOY_STARTED) when detached=1
	if result.Status != 100 && result.Status != 101 {
		return fmt.Errorf("upload app tar failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}

type CandidateBuildReceipt struct {
	Status string `json:"status"`
	Image  string `json:"image"`
	Error  string `json:"error"`
	Log    string `json:"log"`
}

var errCandidateBuildAuthExpired = errors.New("candidate build receipt token expired")

func (c *CapRoverClient) GetCandidateBuild(ctx context.Context, slug, receipt string) (CandidateBuildReceipt, error) {
	endpoint := fmt.Sprintf("%s/api/v2/user/apps/appData/%s/builds/%s", c.baseURL, url.PathEscape(slug), url.PathEscape(receipt))
	var build CandidateBuildReceipt
	token, err := c.getToken(ctx)
	if err != nil {
		return build, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return build, err
	}
	req.Header.Set("x-captain-auth", token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return build, err
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return build, fmt.Errorf("read candidate build receipt: %w", err)
	}
	if result.Status == 1106 || result.Status == 1105 {
		c.invalidateToken()
		return build, errCandidateBuildAuthExpired
	}
	if resp.StatusCode != http.StatusOK || result.Status != 100 {
		return build, fmt.Errorf("candidate build receipt HTTP %d, status %d", resp.StatusCode, result.Status)
	}
	err = json.Unmarshal(result.Data, &build)
	return build, err
}

func (c *CapRoverClient) WaitCandidateImage(ctx context.Context, slug, receipt string) (image, log string, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	interval := c.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastLog string
	for {
		build, err := c.GetCandidateBuild(waitCtx, slug, receipt)
		if build.Log != "" {
			lastLog = build.Log
		}
		switch {
		case errors.Is(err, errCandidateBuildAuthExpired):
			// Fetch a fresh token on the next poll.
		case err != nil:
			return "", lastLog, err
		case build.Status == "built":
			if build.Image == "" {
				return "", lastLog, fmt.Errorf("candidate build has no image")
			}
			return build.Image, lastLog, nil
		case build.Status == "failed":
			return "", lastLog, fmt.Errorf("%s", build.Error)
		case build.Status == "building", build.Status == "unknown":
		default:
			return "", lastLog, fmt.Errorf("invalid candidate build status")
		}
		select {
		case <-waitCtx.Done():
			return "", lastLog, fmt.Errorf("candidate build result is unknown: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// GetAppImageInfo 查询应用在 CapRover 中当前生效的内部版本号（deployedVersion）与构建产出的 Docker 镜像名（如 img-captain-slug:1）
func (c *CapRoverClient) GetAppImageInfo(ctx context.Context, slug string) (int, string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return 0, "", err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v2/user/apps/appDefinitions", nil)
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("x-captain-auth", token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		var result caproverResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return 0, "", decodeErr
		}
		if (result.Status == 1106 || result.Status == 1105) && attempt == 0 {
			c.invalidateToken()
			continue
		}
		if result.Status != 100 {
			return 0, "", fmt.Errorf("get appDefinitions failed (status %d): %s", result.Status, result.Description)
		}
		var data struct {
			AppDefinitions []struct {
				AppName         string `json:"appName"`
				DeployedVersion int    `json:"deployedVersion"`
				Versions        []struct {
					Version           int    `json:"version"`
					DeployedImageName string `json:"deployedImageName"`
				} `json:"versions"`
			} `json:"appDefinitions"`
		}
		if err := json.Unmarshal(result.Data, &data); err != nil {
			return 0, "", err
		}
		for _, app := range data.AppDefinitions {
			if app.AppName == slug {
				ver := app.DeployedVersion
				imgName := ""
				for _, v := range app.Versions {
					if v.Version == ver && strings.TrimSpace(v.DeployedImageName) != "" {
						imgName = strings.TrimSpace(v.DeployedImageName)
						break
					}
				}
				if imgName == "" && ver > 0 {
					imgName = fmt.Sprintf("img-captain-%s:%d", slug, ver)
				}
				return ver, imgName, nil
			}
		}
		return 0, "", fmt.Errorf("app %s not found in CapRover appDefinitions", slug)
	}
	return 0, "", fmt.Errorf("get appDefinitions failed after token refresh")
}

// DeployExistingImage 通过 CapRover captainDefinitionContent 的 imageName 直接部署已存在的历史版本镜像（无需重新编译 Dockerfile，秒级回退）
func (c *CapRoverClient) DeployExistingImage(ctx context.Context, slug string, imageName string) error {
	imageName = strings.TrimSpace(imageName)
	if imageName == "" {
		return fmt.Errorf("empty imageName for DeployExistingImage")
	}
	captainDefBytes, _ := json.Marshal(map[string]interface{}{
		"schemaVersion": 2,
		"imageName":     imageName,
	})
	bodyPayload, _ := json.Marshal(map[string]string{
		"captainDefinitionContent": string(captainDefBytes),
		"gitHash":                  "",
	})

	deployURL := fmt.Sprintf("%s/api/v2/user/apps/appData/%s/?detached=1", c.baseURL, url.PathEscape(slug))
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", deployURL, bytes.NewReader(bodyPayload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-captain-auth", token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("deploy existing image request: %w", err)
		}
		var result caproverResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode deploy existing image response: %w", decodeErr)
		}
		if (result.Status == 1106 || result.Status == 1105) && attempt == 0 {
			c.invalidateToken()
			continue
		}
		if result.Status != 100 && result.Status != 101 {
			return fmt.Errorf("deploy existing image %s failed (status %d): %s", imageName, result.Status, result.Description)
		}
		return nil
	}
	return fmt.Errorf("deploy existing image failed after token refresh")
}

// invalidateToken 清空缓存的 CapRover token，用于 1106 过期重试
func (c *CapRoverClient) invalidateToken() {
	c.mu.Lock()
	c.authToken = ""
	c.mu.Unlock()
}

// RuntimeLogs returns bounded application output for a failed deployment diagnosis.
func (c *CapRoverClient) RuntimeLogs(ctx context.Context, slug string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(ctx)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v2/user/apps/appData/"+url.PathEscape(slug)+"/logs?encoding=utf8", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("x-captain-auth", token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return "", err
		}
		var result caproverResponse
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&result)
		resp.Body.Close()
		if decodeErr != nil {
			return "", decodeErr
		}
		if (result.Status == 1106 || result.Status == 1105) && attempt == 0 {
			c.invalidateToken()
			continue
		}
		if resp.StatusCode != http.StatusOK || result.Status != 100 {
			return "", fmt.Errorf("runtime log request failed: %d", result.Status)
		}
		var data struct {
			Logs string `json:"logs"`
		}
		if err := json.Unmarshal(result.Data, &data); err != nil {
			return "", err
		}
		return data.Logs, nil
	}
	return "", fmt.Errorf("runtime log request failed after token refresh")
}

// formatCapRoverLogLines cleans CapRover's circular buffer log lines (which start with empty "" slots
// and often end with trailing "\n" per element) so joined build logs have no blank gaps.
func formatCapRoverLogLines(lines []string) string {
	cleaned := make([]string, 0, len(lines))
	for _, raw := range lines {
		for _, sub := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
			trimmed := strings.TrimRight(sub, " \t\r")
			if strings.TrimSpace(trimmed) == "" {
				continue
			}
			cleaned = append(cleaned, trimmed)
		}
	}
	return strings.Join(cleaned, "\n")
}

func isAppRootPlaceholder(body []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(body)))
	return strings.Contains(s, "powered by caprover") ||
		strings.Contains(s, "captain-root-domain")
}

// WaitBuildAndAppReady waits for the built container and the application URL.
func (c *CapRoverClient) WaitBuildAndAppReady(ctx context.Context, slug, appURL string) (string, error) {
	return c.WaitBuildAndAppReadyWithCallback(ctx, slug, appURL, nil)
}

// WaitBuildAndAppReadyWithCallback 轮询构建状态并支持实时日志回调
func (c *CapRoverClient) WaitBuildAndAppReadyWithCallback(ctx context.Context, slug, appURL string, onLog func(string)) (string, error) {
	return c.WaitBuildConfigureAndAppReady(ctx, slug, appURL, nil, onLog)
}

// WaitBuildConfigureAndAppReady applies approved configuration after the first image exists,
// before probing a service which may require these values just to start.
func (c *CapRoverClient) WaitBuildConfigureAndAppReady(ctx context.Context, slug, appURL string, configure func() error, onLog func(string)) (string, error) {
	return c.waitBuildConfigureAndRuntime(ctx, slug, appURL, configure, onLog)
}

func (c *CapRoverClient) WaitDeployedRuntime(ctx context.Context, slug, image string) (string, error) {
	timeout := c.StartupTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	interval := c.ProbeInterval
	if interval <= 0 {
		interval = time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	detail, err := waitForAppCondition(waitCtx, interval, func(checkCtx context.Context) (bool, string) {
		return c.checkTargetRuntime(checkCtx, slug, image)
	})
	if err != nil {
		return detail, fmt.Errorf("目标容器在 %s 内未进入运行态：%s: %w", timeout, detail, err)
	}
	return "[Runtime] " + detail, nil
}

func (c *CapRoverClient) waitBuildConfigureAndRuntime(ctx context.Context, slug, appURL string, configure func() error, onLog func(string)) (string, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}

	statusURL := fmt.Sprintf("%s/api/v2/user/apps/appData/%s/", c.baseURL, slug)
	var lastLogs string

	// 1. 轮询构建直至 isAppBuilding == false (最长等待 10 分钟)
	pollInterval := c.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	buildTicker := time.NewTicker(pollInterval)
	defer buildTicker.Stop()
	buildTimeout := time.After(10 * time.Minute)

	buildingDone := false
	for !buildingDone {
		select {
		case <-ctx.Done():
			return lastLogs, ctx.Err()
		case <-buildTimeout:
			return lastLogs, fmt.Errorf("build timeout after 10 minutes")
		case <-buildTicker.C:
			req, err := http.NewRequestWithContext(ctx, "GET", statusURL, nil)
			if err != nil {
				continue
			}
			req.Header.Set("x-captain-auth", token)

			resp, err := c.httpClient.Do(req)
			if err != nil {
				continue
			}

			var result caproverResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				resp.Body.Close()
				continue
			}
			resp.Body.Close()

			if result.Status != 100 {
				// 1106/1105 表示 token 失效，重新登录后继续轮询
				if result.Status == 1106 || result.Status == 1105 {
					c.invalidateToken()
					if newToken, loginErr := c.getToken(ctx); loginErr == nil {
						token = newToken
					}
				}
				continue
			}

			var data struct {
				IsAppBuilding *bool `json:"isAppBuilding"`
				IsBuildFailed bool  `json:"isBuildFailed"`
				Logs          struct {
					Lines []string `json:"lines"`
				} `json:"logs"`
			}
			if err := json.Unmarshal(result.Data, &data); err != nil || data.IsAppBuilding == nil {
				continue
			}
			if len(data.Logs.Lines) > 0 {
				lastLogs = formatCapRoverLogLines(data.Logs.Lines)
				if onLog != nil {
					onLog(lastLogs)
				}
			}

			if !*data.IsAppBuilding {
				buildingDone = true
				if data.IsBuildFailed {
					return lastLogs, fmt.Errorf("container build failed on CapRover")
				}
			}
		}
	}

	if configure != nil {
		if err := configure(); err != nil {
			return lastLogs, fmt.Errorf("apply approved configuration after build: %w", err)
		}
	}

	const defaultStartupTimeout = 90 * time.Second
	startupTimeout := c.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = defaultStartupTimeout
	}
	probeInterval := c.ProbeInterval
	if probeInterval <= 0 {
		probeInterval = time.Second
	}

	startupCtx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	var expectedImage string
	lastRuntime, err := waitForAppCondition(startupCtx, probeInterval, func(checkCtx context.Context) (bool, string) {
		if expectedImage == "" {
			_, image, imageErr := c.GetAppImageInfo(checkCtx, slug)
			if imageErr != nil {
				return false, "查询目标镜像失败：" + imageErr.Error()
			}
			if image == "" {
				return false, "平台尚未返回目标镜像"
			}
			expectedImage = image
		}
		return c.checkTargetRuntime(checkCtx, slug, expectedImage)
	})
	cancelStartup()
	if err != nil {
		if ctx.Err() != nil {
			return lastLogs, ctx.Err()
		}
		return lastLogs, fmt.Errorf("目标容器在 %s 内未进入运行态：%s", startupTimeout, lastRuntime)
	}
	if appURL == "" {
		return lastLogs + "\n[Runtime] " + lastRuntime, nil
	}

	domainLog, err := c.WaitDeployedAppDomain(ctx, slug, expectedImage, appURL)
	return lastLogs + "\n" + domainLog, err
}

// WaitDeployedAppDomain checks the assigned entry point after the target service starts.
func (c *CapRoverClient) WaitDeployedAppDomain(ctx context.Context, slug, image, appURL string) (string, error) {
	timeout := c.AppReadyTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	interval := c.ProbeInterval
	if interval <= 0 {
		interval = time.Second
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, timeout)
	defer cancelReady()
	lastProbe, err := waitForAppCondition(readyCtx, interval, func(checkCtx context.Context) (bool, string) {
		// A successful response still needs to belong to a running target version.
		ok, detail, probeErr := c.ProbeAppEndpoint(checkCtx, appURL)
		if probeErr != nil {
			return false, probeErr.Error()
		}
		if !ok {
			return false, detail
		}
		if running, runtimeDetail := c.checkTargetRuntime(checkCtx, slug, image); !running {
			return false, runtimeDetail
		}
		return true, detail
	})
	if err != nil {
		if ctx.Err() != nil {
			return lastProbe, ctx.Err()
		}
		return lastProbe, fmt.Errorf("目标容器已启动，但应用域名入口在 %s 内未就绪：%s", timeout, lastProbe)
	}
	return "[AppReady] " + lastProbe, nil
}

func waitForAppCondition(ctx context.Context, interval time.Duration, check func(context.Context) (bool, string)) (string, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := "探测未完成"
	for ctx.Err() == nil {
		ok, detail := check(ctx)
		if ctx.Err() != nil {
			break
		}
		last = detail
		if ok {
			return last, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return last, ctx.Err()
}

func (c *CapRoverClient) checkTargetRuntime(ctx context.Context, slug, expectedImage string) (bool, string) {
	runtimeURL := fmt.Sprintf("%s/api/v2/user/apps/runtime/%s", c.baseURL, url.PathEscape(slug))
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.getToken(requestCtx)
		if err != nil {
			return false, "获取部署平台凭据失败"
		}
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, runtimeURL, nil)
		if err != nil {
			return false, "无法构造容器状态查询"
		}
		req.Header.Set("x-captain-auth", token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return false, "无法查询容器运行状态：" + err.Error()
		}
		var result caproverResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			return false, fmt.Sprintf("容器运行状态查询未返回有效响应（HTTP %d）", resp.StatusCode)
		}
		if (result.Status == 1106 || result.Status == 1105) && attempt == 0 {
			c.invalidateToken()
			continue
		}
		if result.Status != 100 {
			return false, fmt.Sprintf("容器运行状态查询失败（状态 %d）", result.Status)
		}
		type runtimeTask struct {
			Image, State, Message, Error string
			ExitCode                     *int
		}
		var data struct {
			DesiredInstances *int
			CurrentTasks     []runtimeTask
			TaskHistory      []runtimeTask
		}
		if err := json.Unmarshal(result.Data, &data); err != nil {
			return false, "容器运行状态响应无法解析"
		}
		if data.DesiredInstances == nil || *data.DesiredInstances <= 0 {
			return false, "目标容器实例数尚不可用或为零"
		}
		matchesImage := func(image string) bool {
			image = strings.TrimSpace(image)
			if !strings.Contains(expectedImage, "@") {
				image = strings.SplitN(image, "@", 2)[0]
			}
			return image == expectedImage
		}
		matching, stale := 0, 0
		for _, task := range data.CurrentTasks {
			if !strings.EqualFold(task.State, "running") {
				continue
			}
			if matchesImage(task.Image) {
				matching++
			} else {
				stale++
			}
		}
		running := matching >= *data.DesiredInstances && stale == 0
		detail := fmt.Sprintf("目标版本实例已运行 %d/%d，旧版本运行实例 %d", matching, *data.DesiredInstances, stale)
		if !running {
			var events []string
			for _, task := range slices.Concat(data.CurrentTasks, data.TaskHistory) {
				if !matchesImage(task.Image) || (task.Message == "" && task.Error == "" && task.ExitCode == nil) {
					continue
				}
				event := strings.TrimSpace(strings.Join([]string{task.State, task.Message, task.Error}, " "))
				if task.ExitCode != nil {
					event += fmt.Sprintf(" (exit %d)", *task.ExitCode)
				}
				if !slices.Contains(events, event) {
					events = append(events, event)
				}
			}
			if len(events) > 0 {
				detail += "\n" + strings.Join(events, "\n")
			}
		}
		return running, detail
	}
	return false, "容器运行状态查询失败"
}

// ProbeAppEndpoint checks the application domain root, with internal ingress as a fallback.
func (c *CapRoverClient) ProbeAppEndpoint(ctx context.Context, appURL string) (bool, string, error) {
	rootURL := strings.TrimRight(appURL, "/") + "/"
	app, err := url.Parse(rootURL)
	if err != nil || app.Hostname() == "" || (app.Scheme != "http" && app.Scheme != "https") {
		return false, "", fmt.Errorf("invalid application URL")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	tryProbe := func(target, host string) (bool, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return false, err.Error()
		}
		if host != "" {
			req.Host = host
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNoContent, http.StatusUnauthorized, http.StatusForbidden:
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location, err := resp.Location()
			// Same-host redirects are followed by the client. An application may
			// hand off authentication to an external identity provider.
			if err != nil || (location.Scheme != "http" && location.Scheme != "https") ||
				strings.EqualFold(location.Hostname(), app.Hostname()) || strings.EqualFold(location.Hostname(), req.URL.Hostname()) {
				return false, fmt.Sprintf("应用入口重定向未完成（%s）", rootURL)
			}
		default:
			return false, fmt.Sprintf("应用入口返回 HTTP %d（%s）", resp.StatusCode, rootURL)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if err != nil {
			return false, "无法读取应用入口响应：" + err.Error()
		}
		if isAppRootPlaceholder(body) || (resp.StatusCode == http.StatusOK && len(bytes.TrimSpace(body)) == 0) {
			return false, "应用入口返回空内容或网关占位页：" + rootURL
		}
		return true, fmt.Sprintf("应用入口返回 HTTP %d：%s", resp.StatusCode, rootURL)
	}

	ok, detail := tryProbe(rootURL, "")
	if ok {
		return true, detail, nil
	}
	if gateway, err := url.Parse(c.baseURL); err == nil {
		host := gateway.Hostname()
		if host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
			target := "http://" + net.JoinHostPort(host, "80") + app.RequestURI()
			if ok, fallbackDetail := tryProbe(target, app.Host); ok {
				return true, fallbackDetail + "（内网网关）", nil
			}
		}
	}
	return false, detail, nil
}

// DeleteApp 在 CapRover 集群中彻底销毁应用容器服务及网络定义
func (c *CapRoverClient) DeleteApp(ctx context.Context, slug string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"appName": slug,
		"volumes": []string{},
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/user/apps/appDefinitions/delete", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("caprover delete app request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode caprover delete response: %w", err)
	}

	// 100 为成功；如果已经不存在也算正常
	if result.Status != 100 && !strings.Contains(strings.ToLower(result.Description), "not found") {
		return fmt.Errorf("caprover delete app failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}
