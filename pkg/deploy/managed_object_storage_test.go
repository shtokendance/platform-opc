package deploy

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Run against a disposable MinIO instance because allocation creates buckets and credentials.
func TestManagedApplicationBuckets(t *testing.T) {
	endpoint := os.Getenv("OPC_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("a disposable MinIO endpoint is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := ApplicationBucketConfig{
		Endpoint: endpoint, KeyID: os.Getenv("OPC_TEST_MINIO_KEY"), Secret: os.Getenv("OPC_TEST_MINIO_SECRET"),
		BucketPrefix: "opc-test-storage",
	}
	first, err := EnsureApplicationBucket(ctx, cfg, "object-resource", "app-one")
	if err != nil {
		t.Fatal(err)
	}
	if first["S3_ACCESS_KEY"] == cfg.KeyID || first["S3_SECRET_KEY"] == cfg.Secret {
		t.Fatal("application received provisioning credentials")
	}
	second, err := EnsureApplicationBucket(ctx, cfg, "object-resource", "app-two")
	if err != nil {
		t.Fatal(err)
	}
	if first["S3_BUCKET"] == second["S3_BUCKET"] || first["S3_ACCESS_KEY"] == second["S3_ACCESS_KEY"] {
		t.Fatal("applications share an identity or bucket")
	}
	access := ArtifactStoreConfig{Endpoint: endpoint, AccessKey: first["S3_ACCESS_KEY"], SecretKey: first["S3_SECRET_KEY"], Bucket: first["S3_BUCKET"]}
	store := NewArtifactStore(access)
	if err := store.putS3Object(ctx, "example", []byte("persistent"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	reused, err := EnsureApplicationBucket(ctx, cfg, "object-resource", "app-one")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != fmt.Sprint(reused) {
		t.Fatal("repeat allocation changed bucket or credentials")
	}
	data, _, err := store.getS3Object(ctx, "example")
	if err != nil || string(data) != "persistent" {
		t.Fatalf("read existing object: %v", err)
	}
	access.Bucket = second["S3_BUCKET"]
	other := NewArtifactStore(access)
	if err := other.putS3Object(ctx, "cross-application", []byte("unexpected"), "text/plain"); err == nil {
		t.Fatal("application credential can write into another application's bucket")
	}
}
