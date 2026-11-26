package workopc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalStorePutServeRangeAndDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{LocalDir: dir})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if store.BackendName() != "local" {
		t.Fatalf("expected local backend, got %s", store.BackendName())
	}

	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	key := BuildObjectKey("stable", "0.2.3-alpha.2", "mac-arm64", "WorkOPC-0.2.3-alpha.2-mac-arm64.dmg")
	res, err := store.PutStream(context.Background(), key, bytes.NewReader(payload), int64(len(payload)), "")
	if err != nil {
		t.Fatalf("put stream: %v", err)
	}
	if res.SizeBytes != int64(len(payload)) {
		t.Fatalf("size mismatch: got %d want %d", res.SizeBytes, len(payload))
	}
	want512 := sha512.Sum512(payload)
	if res.SHA512 != base64.StdEncoding.EncodeToString(want512[:]) {
		t.Fatalf("sha512 mismatch: got %s", res.SHA512)
	}
	want256 := sha256.Sum256(payload)
	if res.SHA256 != hex.EncodeToString(want256[:]) {
		t.Fatalf("sha256 mismatch: got %s", res.SHA256)
	}

	req := httptest.NewRequest(http.MethodGet, "/download", nil)
	rec := httptest.NewRecorder()
	if err := store.ServeObject(context.Background(), rec, req, key, "WorkOPC.dmg", ""); err != nil {
		t.Fatalf("serve full: %v", err)
	}
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("full serve failed: code=%d body=%q", rec.Code, rec.Body.String())
	}

	rangeReq := httptest.NewRequest(http.MethodGet, "/download", nil)
	rangeReq.Header.Set("Range", "bytes=10-15")
	rangeRec := httptest.NewRecorder()
	if err := store.ServeObject(context.Background(), rangeRec, rangeReq, key, "", ""); err != nil {
		t.Fatalf("serve range: %v", err)
	}
	if rangeRec.Code != http.StatusPartialContent || rangeRec.Body.String() != "abcdef" {
		t.Fatalf("range serve failed: code=%d body=%q", rangeRec.Code, rangeRec.Body.String())
	}

	if err := store.DeleteObject(context.Background(), key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	missRec := httptest.NewRecorder()
	err = store.ServeObject(context.Background(), missRec, httptest.NewRequest(http.MethodGet, "/download", nil), key, "", "")
	if err != ErrArtifactNotFound {
		t.Fatalf("expected ErrArtifactNotFound after delete, got %v", err)
	}
}

func TestParseByteRange(t *testing.T) {
	start, end, hasRange, err := parseByteRange("bytes=0-9", 100)
	if err != nil || !hasRange || start != 0 || end != 9 {
		t.Fatalf("unexpected range result: %d-%d %v %v", start, end, hasRange, err)
	}
	start, end, hasRange, err = parseByteRange("bytes=-10", 100)
	if err != nil || !hasRange || start != 90 || end != 99 {
		t.Fatalf("unexpected suffix range: %d-%d %v %v", start, end, hasRange, err)
	}
	if _, _, _, err := parseByteRange("bytes=200-300", 100); err == nil {
		t.Fatalf("expected out-of-bounds error")
	}
}
