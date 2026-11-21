package handler

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"weknora-tokendance/pkg/model"
)

func TestDeliveryPlanValidatesVersionAndExactApplicationFiles(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	dockerfile := "FROM node:20\n"
	if err := writer.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0644, Size: int64(len(dockerfile))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(dockerfile)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(dockerfile))
	plan := []byte(fmt.Sprintf(`{"format":1,"slug":"example","base_version":2,"summary":"deployment update","contract":{"slug":"example","runtime":{"port":8080},"storage_mounts":[]},"files":[{"path":"Dockerfile","sha256":"%x"}]}`, sum))
	if err := validateDeliveryPlan(plan, "example", 8080, 2, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := validateDeliveryPlan(plan, "example", 8080, 3, archive.Bytes()); err == nil {
		t.Fatal("stale base version accepted")
	}
	if err := validateDeliveryPlan(plan, "example", 8080, 2, bytes.ReplaceAll(archive.Bytes(), []byte("node"), []byte("ruby"))); err == nil {
		t.Fatal("altered application file accepted")
	}
	if got := buildDeploymentArchive(&model.AppDeploymentVersion{DeliveryPlanSHA256: fmt.Sprintf("%x", sha256.Sum256(plan))}, archive.Bytes(), []model.AppStorageMount{{ContainerPath: "/app/data"}}); !bytes.Equal(got, archive.Bytes()) {
		t.Fatal("skill-driven archive was rewritten")
	}
	content, err := readDeliveryArchiveFile(archive.Bytes(), "Dockerfile")
	if err != nil || content != dockerfile {
		t.Fatalf("archived file: %q, error: %v", content, err)
	}
}

func TestPublishArchivesSkillDeliveryWithExplicitEmptyMounts(t *testing.T) {
	h, r := setupTestAppHandler(t)
	const slug = "skill-delivery"
	var archive bytes.Buffer
	tarWriter := tar.NewWriter(&archive)
	for name, content := range map[string]string{
		"Dockerfile":      "FROM node:20\nWORKDIR /app\nCOPY . .\n",
		"data/state.json": "{}",
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	plan := []byte(`{"format":1,"slug":"skill-delivery","base_version":0,"summary":"deployment","contract":{"slug":"skill-delivery","runtime":{"port":8080},"storage_mounts":[]},"files":[]}`)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range map[string]string{"slug": slug, "name": slug, "storage_mounts": "[]", "delivery_plan": string(plan)} {
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for filename, data := range map[string][]byte{"app_tar": archive.Bytes(), "skill_zip": []byte("test skill")} {
		file, err := form.CreateFormFile(filename, filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/apps/publish", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("publish returned %d: %s", response.Code, response.Body.String())
	}
	var receipt struct {
		AttemptID string                  `json:"attempt_id"`
		PlanHash  string                  `json:"delivery_plan_sha256"`
		Mounts    []model.AppStorageMount `json:"storage_mounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.PlanHash != fmt.Sprintf("%x", sha256.Sum256(plan)) || len(receipt.Mounts) != 0 {
		t.Fatalf("unexpected delivery receipt: %+v", receipt)
	}
	data, _, err := h.resolveArtifactStore(t.Context()).GetObject(t.Context(), "apps/"+slug+"/attempts/"+receipt.AttemptID+"/delivery.json")
	if err != nil || !bytes.Equal(data, plan) {
		t.Fatalf("archived manifest differs from submitted plan: %v", err)
	}
}
