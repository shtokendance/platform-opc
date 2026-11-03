package deploy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ArtifactStoreConfig 定义应用版本物理制品（app.tar / skill.zip / Dockerfile / manifest.json）的存储配置。
// 优先使用 MinIO / S3 对象存储，同时支持本地目录作为回退与缓存。
type ArtifactStoreConfig struct {
	Endpoint  string // 例如 http://10.101.102.116:9000
	AccessKey string // 例如 minioadmin
	SecretKey string // 例如 Ad123456
	Bucket    string // 默认为 opc-apps
	Region    string // 默认为 us-east-1
	LocalDir  string // 本地兜底存储目录，默认 /tmp/opc-deploy-artifacts
}

// ArtifactStore 提供统一的物理文件存取接口（底层对接 MinIO S3 API + 本地兜底）
type ArtifactStore struct {
	cfg           ArtifactStoreConfig
	httpClient    *http.Client
	mu            sync.Mutex
	bucketChecked bool
}

func NewArtifactStore(cfg ArtifactStoreConfig) *ArtifactStore {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	if cfg.Bucket == "" {
		cfg.Bucket = "opc-apps"
	}
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.LocalDir = strings.TrimSpace(cfg.LocalDir)
	if cfg.LocalDir == "" {
		cfg.LocalDir = filepath.Join(os.TempDir(), "opc-deploy-artifacts")
	}
	return &ArtifactStore{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *ArtifactStore) Bucket() string {
	return s.cfg.Bucket
}

func (s *ArtifactStore) HasS3() bool {
	return s.cfg.Endpoint != "" && s.cfg.AccessKey != "" && s.cfg.SecretKey != ""
}

// BuildVersionObjectKey 生成统一的版本物理文件对象路径：apps/<slug>/v<version>/<filename>
func BuildVersionObjectKey(slug string, version int, filename string) string {
	cleanSlug := strings.Trim(strings.ToLower(strings.TrimSpace(slug)), "/")
	cleanFile := filepath.Base(strings.TrimSpace(filename))
	return fmt.Sprintf("apps/%s/v%d/%s", cleanSlug, version, cleanFile)
}

// PutObject 将物理文件写入 MinIO（若已配置），并同步写入本地兜底目录
func (s *ArtifactStore) PutObject(ctx context.Context, objectKey string, data []byte, contentType string) error {
	cleanKey := strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		return fmt.Errorf("empty object key")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// 1. 始终保存一份到本地兜底缓存目录，确保单测或离线环境立即可读
	localErr := s.putLocalObject(cleanKey, data)

	// 2. 若配置了 MinIO / S3，上传至 MinIO Bucket
	if s.HasS3() {
		if err := s.ensureBucket(ctx); err != nil {
			if localErr == nil {
				return fmt.Errorf("minio ensure bucket failed (%v), saved to local fallback", err)
			}
			return err
		}
		if err := s.putS3Object(ctx, cleanKey, data, contentType); err != nil {
			return fmt.Errorf("minio put object %s failed: %w", cleanKey, err)
		}
		return nil
	}

	return localErr
}

// GetObject 从 MinIO 读取物理文件字节流；若 MinIO 未配置或读取失败，则自动回退读取本地兜底目录
func (s *ArtifactStore) GetObject(ctx context.Context, objectKey string) ([]byte, string, error) {
	cleanKey := strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		return nil, "", fmt.Errorf("empty object key")
	}

	if s.HasS3() {
		data, ct, err := s.getS3Object(ctx, cleanKey)
		if err == nil {
			return data, ct, nil
		}
		// 若 MinIO 读取失败，尝试从本地兜底目录恢复
		if localData, localErr := s.getLocalObject(cleanKey); localErr == nil {
			return localData, inferContentType(cleanKey), nil
		}
		return nil, "", err
	}

	data, err := s.getLocalObject(cleanKey)
	if err != nil {
		return nil, "", err
	}
	return data, inferContentType(cleanKey), nil
}

// ObjectExists 检查对象是否存在于 MinIO 或本地存储中
func (s *ArtifactStore) ObjectExists(ctx context.Context, objectKey string) bool {
	cleanKey := strings.TrimLeft(strings.TrimSpace(objectKey), "/")
	if cleanKey == "" {
		return false
	}
	if s.HasS3() {
		if s.headS3Object(ctx, cleanKey) == nil {
			return true
		}
	}
	p, err := s.safeLocalPath(cleanKey)
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func (s *ArtifactStore) safeLocalPath(objectKey string) (string, error) {
	base := filepath.Clean(s.cfg.LocalDir)
	full := filepath.Clean(filepath.Join(base, filepath.FromSlash(objectKey)))
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid object key path: %s", objectKey)
	}
	return full, nil
}

func (s *ArtifactStore) putLocalObject(objectKey string, data []byte) error {
	target, err := s.safeLocalPath(objectKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

func (s *ArtifactStore) getLocalObject(objectKey string) ([]byte, error) {
	target, err := s.safeLocalPath(objectKey)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

func inferContentType(objectKey string) string {
	lower := strings.ToLower(objectKey)
	switch {
	case strings.HasSuffix(lower, ".tar"):
		return "application/x-tar"
	case strings.HasSuffix(lower, ".zip"):
		return "application/zip"
	case strings.HasSuffix(lower, ".json"):
		return "application/json; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

func (s *ArtifactStore) ensureBucket(ctx context.Context) error {
	s.mu.Lock()
	if s.bucketChecked {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	bucketPath := "/" + url.PathEscape(s.cfg.Bucket)
	req, err := s.newSignedRequest(ctx, http.MethodHead, bucketPath, nil, "")
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		s.mu.Lock()
		s.bucketChecked = true
		s.mu.Unlock()
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		createReq, err := s.newSignedRequest(ctx, http.MethodPut, bucketPath, nil, "")
		if err != nil {
			return err
		}
		createResp, err := s.httpClient.Do(createReq)
		if err != nil {
			return err
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(createResp.Body, 4096))
		_ = createResp.Body.Close()
		// 200 OK 或 409 BucketAlreadyOwnedByYou 均视为成功
		if createResp.StatusCode == http.StatusOK || createResp.StatusCode == http.StatusNoContent || createResp.StatusCode == http.StatusConflict {
			s.mu.Lock()
			s.bucketChecked = true
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("create bucket %s returned status %d: %s", s.cfg.Bucket, createResp.StatusCode, string(bodyBytes))
	}

	return fmt.Errorf("head bucket %s returned status %d", s.cfg.Bucket, resp.StatusCode)
}

func (s *ArtifactStore) putS3Object(ctx context.Context, objectKey string, data []byte, contentType string) error {
	escapedKey := escapeS3ObjectPath(objectKey)
	uriPath := "/" + url.PathEscape(s.cfg.Bucket) + "/" + escapedKey
	req, err := s.newSignedRequest(ctx, http.MethodPut, uriPath, data, contentType)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func (s *ArtifactStore) getS3Object(ctx context.Context, objectKey string) ([]byte, string, error) {
	escapedKey := escapeS3ObjectPath(objectKey)
	uriPath := "/" + url.PathEscape(s.cfg.Bucket) + "/" + escapedKey
	req, err := s.newSignedRequest(ctx, http.MethodGet, uriPath, nil, "")
	if err != nil {
		return nil, "", err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("minio get %s status %d: %s", objectKey, resp.StatusCode, string(bodyBytes))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		ct = inferContentType(objectKey)
	}
	return data, ct, nil
}

func (s *ArtifactStore) headS3Object(ctx context.Context, objectKey string) error {
	escapedKey := escapeS3ObjectPath(objectKey)
	uriPath := "/" + url.PathEscape(s.cfg.Bucket) + "/" + escapedKey
	req, err := s.newSignedRequest(ctx, http.MethodHead, uriPath, nil, "")
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func escapeS3ObjectPath(objectKey string) string {
	parts := strings.Split(strings.TrimLeft(objectKey, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// newSignedRequest 使用 AWS Signature Version 4 对 MinIO / S3 HTTP 请求进行签名
func (s *ArtifactStore) newSignedRequest(ctx context.Context, method string, canonicalURI string, payload []byte, contentType string) (*http.Request, error) {
	fullURL := s.cfg.Endpoint + canonicalURI
	var bodyReader io.Reader
	if len(payload) > 0 {
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		req.ContentLength = int64(len(payload))
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	hostHeader := req.URL.Host
	req.Host = hostHeader

	signedHeadersMap := map[string]string{
		"host":                 hostHeader,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if contentType != "" {
		signedHeadersMap["content-type"] = contentType
	}

	headerKeys := make([]string, 0, len(signedHeadersMap))
	for k := range signedHeadersMap {
		headerKeys = append(headerKeys, k)
	}
	sort.Strings(headerKeys)

	var canonicalHeaders strings.Builder
	for _, k := range headerKeys {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(signedHeadersMap[k]))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(headerKeys, ";")

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		"", // query string
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.cfg.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveS3SigningKey(s.cfg.SecretKey, dateStamp, s.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey,
		credentialScope,
		signedHeaders,
		signature,
	)
	req.Header.Set("Authorization", authHeader)
	return req, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func deriveS3SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}
