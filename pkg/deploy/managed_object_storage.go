package deploy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"

	"github.com/minio/madmin-go/v3"
)

type ApplicationBucketConfig struct {
	Endpoint     string
	KeyID        string
	Secret       string
	BucketPrefix string
}

var appBucketPrefix = regexp.MustCompile(`^[a-z][a-z0-9-]{1,22}[a-z0-9]$`)

func (cfg ApplicationBucketConfig) Validate() error {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("managed MinIO requires an HTTP(S) endpoint without credentials or a path")
	}
	if cfg.KeyID == "" || cfg.Secret == "" || !appBucketPrefix.MatchString(cfg.BucketPrefix) {
		return fmt.Errorf("managed MinIO requires provisioning credentials and a 3–24 character bucket prefix")
	}
	return nil
}

func EnsureApplicationBucket(ctx context.Context, cfg ApplicationBucketConfig, resourceID, appID string) (map[string]string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	endpoint, _ := url.Parse(cfg.Endpoint)
	admin, err := madmin.New(endpoint.Host, cfg.KeyID, cfg.Secret, endpoint.Scheme == "https")
	if err != nil {
		return nil, fmt.Errorf("cannot initialize MinIO provisioning client")
	}

	bucket, access, secret := applicationBucketIdentity(cfg, resourceID, appID)
	arn := "arn:aws:s3:::" + bucket
	policy, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Action": []string{"s3:GetBucketLocation", "s3:ListBucket", "s3:ListBucketMultipartUploads"}, "Resource": []string{arn}},
			{"Effect": "Allow", "Action": []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}, "Resource": []string{arn + "/*"}},
		},
	})
	if err != nil {
		return nil, err
	}
	store := NewArtifactStore(ArtifactStoreConfig{Endpoint: cfg.Endpoint, AccessKey: cfg.KeyID, SecretKey: cfg.Secret, Bucket: bucket})
	if err := store.ensureBucket(ctx); err != nil {
		return nil, fmt.Errorf("cannot provision application bucket")
	}

	info, err := admin.InfoServiceAccount(ctx, access)
	if err != nil {
		var response madmin.ErrorResponse
		if !errors.As(err, &response) || response.Code != "XMinioInvalidIAMCredentials" {
			return nil, fmt.Errorf("cannot inspect application MinIO identity")
		}
		if _, err := admin.AddServiceAccount(ctx, madmin.AddServiceAccountReq{
			TargetUser: cfg.KeyID, AccessKey: access, SecretKey: secret, Policy: policy, Name: access,
		}); err != nil {
			return nil, fmt.Errorf("cannot create scoped application MinIO identity")
		}
	} else {
		var actual, expected any
		_ = json.Unmarshal(policy, &expected)
		if info.ParentUser != cfg.KeyID || info.ImpliedPolicy || info.AccountStatus != "on" ||
			json.Unmarshal([]byte(info.Policy), &actual) != nil || !sameBucketPolicy(actual, expected) {
			return nil, fmt.Errorf("existing application MinIO identity has a different scope")
		}
	}
	// A read-only signed request proves the derived credential still works.
	scoped := NewArtifactStore(ArtifactStoreConfig{Endpoint: cfg.Endpoint, AccessKey: access, SecretKey: secret, Bucket: bucket})
	req, err := scoped.newSignedRequest(ctx, "HEAD", "/"+bucket, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := scoped.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("application bucket is not reachable")
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("application bucket credentials were rejected (%d)", resp.StatusCode)
	}

	return map[string]string{
		"S3_ENDPOINT": cfg.Endpoint, "AWS_ENDPOINT_URL_S3": cfg.Endpoint,
		"S3_BUCKET": bucket, "S3_ACCESS_KEY": access, "AWS_ACCESS_KEY_ID": access,
		"S3_SECRET_KEY": secret, "AWS_SECRET_ACCESS_KEY": secret,
		"S3_REGION": "us-east-1", "AWS_REGION": "us-east-1", "AWS_DEFAULT_REGION": "us-east-1",
		"S3_FORCE_PATH_STYLE": "true",
	}, nil
}

// MinIO serializes single-element Action/Resource sets as strings and can reorder sets.
func sameBucketPolicy(actual, expected any) bool {
	normalize := func(raw any) (map[string]bool, bool) {
		p, ok := raw.(map[string]any)
		if !ok || p["Version"] != "2012-10-17" {
			return nil, false
		}
		list, ok := p["Statement"].([]any)
		if !ok || len(list) != 2 {
			return nil, false
		}
		keys := make(map[string]bool)
		for _, value := range list {
			stmt, ok := value.(map[string]any)
			if !ok || stmt["Effect"] != "Allow" || (len(stmt) != 3 && !(len(stmt) == 4 && stmt["Sid"] == "")) {
				return nil, false
			}
			for _, resource := range policyValues(stmt["Resource"]) {
				for _, action := range policyValues(stmt["Action"]) {
					keys[resource+":"+action] = true
				}
			}
		}
		return keys, true
	}
	a, aok := normalize(actual)
	e, eok := normalize(expected)
	return aok && eok && reflect.DeepEqual(a, e)
}

func policyValues(v any) []string {
	switch values := v.(type) {
	case string:
		return []string{values}
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func applicationBucketIdentity(cfg ApplicationBucketConfig, resourceID, appID string) (bucket, access, secret string) {
	identity := cfg.BucketPrefix + "/" + resourceID + "/" + appID
	sum := sha256.Sum256([]byte(identity))
	suffix := hex.EncodeToString(sum[:16])
	bucket = cfg.BucketPrefix + "-" + suffix
	access = "opc" + suffix[:16]
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	mac.Write([]byte("opc-application-storage/" + identity))
	secret = hex.EncodeToString(mac.Sum(nil)[:20])
	return
}

func ApplicationBucketName(prefix, resourceID, appID string) string {
	bucket, _, _ := applicationBucketIdentity(ApplicationBucketConfig{BucketPrefix: prefix}, resourceID, appID)
	return bucket
}
