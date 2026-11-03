package deploy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestArtifactStoreS3AndFallback(t *testing.T) {
	var mu sync.Mutex
	buckets := map[string]bool{}
	objects := map[string][]byte{}
	contentTypes := map[string]string{}

	s3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=minioadmin/") {
			http.Error(w, "missing or invalid AWS4 signature", http.StatusForbidden)
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		bucket := parts[0]
		key := ""
		if len(parts) > 1 {
			key = parts[1]
		}

		mu.Lock()
		defer mu.Unlock()

		if key == "" {
			switch r.Method {
			case http.MethodHead:
				if buckets[bucket] {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			case http.MethodPut:
				buckets[bucket] = true
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}

		fullKey := bucket + "/" + key
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			objects[fullKey] = data
			contentTypes[fullKey] = r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := objects[fullKey]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if ct := contentTypes[fullKey]; ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		case http.MethodHead:
			if _, ok := objects[fullKey]; ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer s3Server.Close()

	store := NewArtifactStore(ArtifactStoreConfig{
		Endpoint:  s3Server.URL,
		AccessKey: "minioadmin",
		SecretKey: "Ad123456",
		Bucket:    "opc-apps",
		LocalDir:  t.TempDir(),
	})

	ctx := context.Background()
	tarKey := BuildVersionObjectKey("demo-app", 1, "app.tar")
	if tarKey != "apps/demo-app/v1/app.tar" {
		t.Fatalf("unexpected key: %s", tarKey)
	}
	tarContent := []byte("fake-tar-binary-v1")
	if err := store.PutObject(ctx, tarKey, tarContent, "application/x-tar"); err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}
	if !store.ObjectExists(ctx, tarKey) {
		t.Fatalf("expected ObjectExists true for %s", tarKey)
	}

	gotBytes, gotCT, err := store.GetObject(ctx, tarKey)
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	if !bytes.Equal(gotBytes, tarContent) {
		t.Fatalf("GetObject content mismatch: got %q, want %q", string(gotBytes), string(tarContent))
	}
	if gotCT != "application/x-tar" {
		t.Fatalf("unexpected content-type: %s", gotCT)
	}
}
