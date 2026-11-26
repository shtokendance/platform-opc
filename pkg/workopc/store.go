package workopc

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrArtifactNotFound = errors.New("workopc artifact not found")

type StoreConfig struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Bucket          string
	LocalDir        string
}

type PutResult struct {
	ObjectKey string
	SizeBytes int64
	SHA512    string
	SHA256    string
}

type ObjectStore interface {
	BackendName() string
	BucketName() string
	PutStream(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) (*PutResult, error)
	ServeObject(ctx context.Context, w http.ResponseWriter, r *http.Request, objectKey string, downloadFilename string, contentType string) error
	DeleteObject(ctx context.Context, objectKey string) error
}

type Store struct {
	cfg           StoreConfig
	minioClient   *minio.Client
	mu            sync.Mutex
	bucketChecked bool
}

func NewStore(cfg StoreConfig) (*Store, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.AccessKeyID = strings.TrimSpace(cfg.AccessKeyID)
	cfg.SecretAccessKey = strings.TrimSpace(cfg.SecretAccessKey)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	if cfg.Bucket == "" {
		cfg.Bucket = "workopc-releases"
	}
	cfg.LocalDir = strings.TrimSpace(cfg.LocalDir)
	if cfg.LocalDir == "" {
		cfg.LocalDir = filepath.Join("data", "workopc-releases")
	}

	s := &Store{cfg: cfg}
	if cfg.Endpoint != "" && cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		endpoint, useSSL := normalizeMinIOEndpoint(cfg.Endpoint, cfg.UseSSL)
		client, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
			Secure: useSSL,
		})
		if err != nil {
			return nil, fmt.Errorf("init minio client: %w", err)
		}
		s.minioClient = client
	}
	return s, nil
}

func normalizeMinIOEndpoint(raw string, defaultSSL bool) (string, bool) {
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host, strings.EqualFold(u.Scheme, "https")
		}
	}
	return strings.TrimRight(raw, "/"), defaultSSL
}

func BuildObjectKey(channel, version, target, filename string) string {
	cleanChannel := sanitizeSegment(channel)
	cleanVersion := sanitizeSegment(version)
	cleanTarget := sanitizeSegment(target)
	cleanFile := filepath.Base(strings.TrimSpace(filename))
	return path.Join("releases", cleanChannel, cleanVersion, cleanTarget, cleanFile)
}

func sanitizeSegment(v string) string {
	v = strings.TrimSpace(v)
	v = strings.ReplaceAll(v, "/", "_")
	v = strings.ReplaceAll(v, "\\", "_")
	v = strings.ReplaceAll(v, "..", "_")
	if v == "" {
		return "unknown"
	}
	return v
}

func (s *Store) BackendName() string {
	if s.minioClient != nil {
		return "minio"
	}
	return "local"
}

func (s *Store) BucketName() string {
	if s.minioClient != nil {
		return s.cfg.Bucket
	}
	return s.cfg.LocalDir
}

func (s *Store) ensureBucket(ctx context.Context) error {
	if s.minioClient == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bucketChecked {
		return nil
	}
	exists, err := s.minioClient.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("check minio bucket %s: %w", s.cfg.Bucket, err)
	}
	if !exists {
		if err := s.minioClient.MakeBucket(ctx, s.cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			existsAgain, checkErr := s.minioClient.BucketExists(ctx, s.cfg.Bucket)
			if checkErr != nil || !existsAgain {
				return fmt.Errorf("create minio bucket %s: %w", s.cfg.Bucket, err)
			}
		}
	}
	s.bucketChecked = true
	return nil
}

func (s *Store) safeLocalPath(objectKey string) (string, error) {
	clean := path.Clean("/" + strings.TrimSpace(objectKey))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." || strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid object key")
	}
	baseAbs, err := filepath.Abs(s.cfg.LocalDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(baseAbs, filepath.FromSlash(clean))
	rel, err := filepath.Rel(baseAbs, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("object key escapes local dir")
	}
	return full, nil
}

func (s *Store) PutStream(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) (*PutResult, error) {
	cleanKey := strings.TrimLeft(path.Clean("/"+strings.TrimSpace(objectKey)), "/")
	if cleanKey == "" || cleanKey == "." {
		return nil, fmt.Errorf("empty object key")
	}
	if contentType == "" {
		contentType = InferContentType(cleanKey)
	}

	h512 := sha512.New()
	h256 := sha256.New()
	var written int64

	if s.minioClient != nil {
		if err := s.ensureBucket(ctx); err != nil {
			return nil, err
		}
		cr := &countingHashReader{r: reader, h512: h512, h256: h256}
		opts := minio.PutObjectOptions{ContentType: contentType}
		info, err := s.minioClient.PutObject(ctx, s.cfg.Bucket, cleanKey, cr, size, opts)
		if err != nil {
			return nil, fmt.Errorf("minio put object: %w", err)
		}
		written = info.Size
		if written == 0 {
			written = cr.n
		}
	} else {
		dstPath, err := s.safeLocalPath(cleanKey)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return nil, err
		}
		tmpFile, err := os.CreateTemp(filepath.Dir(dstPath), ".upload-*")
		if err != nil {
			return nil, err
		}
		tmpPath := tmpFile.Name()
		mw := io.MultiWriter(tmpFile, h512, h256)
		n, copyErr := io.Copy(mw, reader)
		closeErr := tmpFile.Close()
		if copyErr != nil {
			_ = os.Remove(tmpPath)
			return nil, copyErr
		}
		if closeErr != nil {
			_ = os.Remove(tmpPath)
			return nil, closeErr
		}
		if err := os.Rename(tmpPath, dstPath); err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		written = n
	}

	return &PutResult{
		ObjectKey: cleanKey,
		SizeBytes: written,
		SHA512:    base64.StdEncoding.EncodeToString(h512.Sum(nil)),
		SHA256:    hex.EncodeToString(h256.Sum(nil)),
	}, nil
}

func (s *Store) ServeObject(ctx context.Context, w http.ResponseWriter, r *http.Request, objectKey string, downloadFilename string, contentType string) error {
	cleanKey := strings.TrimLeft(path.Clean("/"+strings.TrimSpace(objectKey)), "/")
	if contentType == "" {
		contentType = InferContentType(cleanKey)
	}
	if downloadFilename != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(downloadFilename)))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", contentType)

	if s.minioClient != nil {
		stat, err := s.minioClient.StatObject(ctx, s.cfg.Bucket, cleanKey, minio.StatObjectOptions{})
		if err != nil {
			errResp := minio.ToErrorResponse(err)
			if errResp.Code == "NoSuchKey" || errResp.StatusCode == http.StatusNotFound {
				return ErrArtifactNotFound
			}
			return err
		}
		totalSize := stat.Size
		start, end, hasRange, err := parseByteRange(r.Header.Get("Range"), totalSize)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return nil
		}
		getOpts := minio.GetObjectOptions{}
		if hasRange {
			if err := getOpts.SetRange(start, end); err != nil {
				return err
			}
		}
		obj, err := s.minioClient.GetObject(ctx, s.cfg.Bucket, cleanKey, getOpts)
		if err != nil {
			return err
		}
		defer obj.Close()

		if hasRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(totalSize, 10))
			w.WriteHeader(http.StatusOK)
		}
		if r.Method == http.MethodHead {
			return nil
		}
		_, err = io.Copy(w, obj)
		return err
	}

	localPath, err := s.safeLocalPath(cleanKey)
	if err != nil {
		return ErrArtifactNotFound
	}
	f, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrArtifactNotFound
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	http.ServeContent(w, r, filepath.Base(cleanKey), info.ModTime(), f)
	return nil
}

func (s *Store) DeleteObject(ctx context.Context, objectKey string) error {
	cleanKey := strings.TrimLeft(path.Clean("/"+strings.TrimSpace(objectKey)), "/")
	if s.minioClient != nil {
		err := s.minioClient.RemoveObject(ctx, s.cfg.Bucket, cleanKey, minio.RemoveObjectOptions{})
		if err != nil {
			errResp := minio.ToErrorResponse(err)
			if errResp.Code == "NoSuchKey" || errResp.StatusCode == http.StatusNotFound {
				return nil
			}
		}
		return err
	}
	localPath, err := s.safeLocalPath(cleanKey)
	if err != nil {
		return nil
	}
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func InferContentType(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".dmg"):
		return "application/x-apple-diskimage"
	case strings.HasSuffix(lower, ".zip"):
		return "application/zip"
	case strings.HasSuffix(lower, ".exe"):
		return "application/vnd.microsoft.portable-executable"
	case strings.HasSuffix(lower, ".blockmap"):
		return "application/octet-stream"
	case strings.HasSuffix(lower, ".yml"), strings.HasSuffix(lower, ".yaml"):
		return "text/yaml; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func parseByteRange(header string, totalSize int64) (int64, int64, bool, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, totalSize - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, fmt.Errorf("unsupported range unit")
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		spec = strings.SplitN(spec, ",", 2)[0]
	}
	parts := strings.SplitN(strings.TrimSpace(spec), "-", 2)
	if len(parts) != 2 || totalSize <= 0 {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, fmt.Errorf("invalid suffix range")
		}
		if suffix >= totalSize {
			return 0, totalSize - 1, true, nil
		}
		return totalSize - suffix, totalSize - 1, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= totalSize {
		return 0, 0, false, fmt.Errorf("range start out of bounds")
	}
	if parts[1] == "" {
		return start, totalSize - 1, true, nil
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start {
		return 0, 0, false, fmt.Errorf("invalid range end")
	}
	if end >= totalSize {
		end = totalSize - 1
	}
	return start, end, true, nil
}

type countingHashReader struct {
	r    io.Reader
	h512 io.Writer
	h256 io.Writer
	n    int64
}

func (c *countingHashReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		_, _ = c.h512.Write(p[:n])
		_, _ = c.h256.Write(p[:n])
	}
	return n, err
}
