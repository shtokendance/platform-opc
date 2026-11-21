package handler

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"weknora-tokendance/pkg/deploy"
)

func TestArchivedDeliveryBuildAndRuntimeAssessments(t *testing.T) {
	h, _ := setupTestAppHandler(t)
	const slug = "fresh-app"
	const image = "img-captain-fresh-app:1"
	var starts atomic.Int32
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/login":
			_, _ = w.Write([]byte(`{"status":100,"data":{"token":"fixture"}}`))
		case r.URL.Path == "/api/v2/user/apps/appDefinitions" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"appDefinitions":[{"appName":%q,"deployedVersion":1,"versions":[{"version":1,"deployedImageName":%q}]}]}}`, slug, image)
		case strings.Contains(r.URL.Path, "/builds/") && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"status":"built","image":%q}}`, image)
		case strings.Contains(r.URL.Path, "/builds/"):
			_, _ = w.Write([]byte(`{"status":101}`))
		case strings.Contains(r.URL.Path, "/runtime/"):
			_, _ = fmt.Fprintf(w, `{"status":100,"data":{"desiredInstances":1,"currentTasks":[{"image":%q,"state":"running"}]}}`, image)
		case strings.Contains(r.URL.Path, "/appData/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"status":100,"data":{"isAppBuilding":false,"isBuildFailed":false}}`))
		case strings.Contains(r.URL.Path, "/appData/"):
			starts.Add(1)
			_, _ = w.Write([]byte(`{"status":101}`))
		default:
			_, _ = w.Write([]byte(`{"status":100}`))
		}
	}))
	defer cap.Close()
	client := deploy.NewCapRoverClient(cap.URL, "cap-password", "apps.opc.intra")
	client.PollInterval = time.Millisecond
	h.SetCapRoverClient(client)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(appOwnerKey, enterpriseAppOwner("fixture", "owner")) })
	router.POST("/deliveries", h.SaveAppDeliveryRevision)
	router.POST("/publish", h.PublishApp)
	router.GET("/:slug/requests/:request_id", h.GetAppPublishRequest)
	router.POST("/:slug/requests/:request_id/assessment", h.AssessAppDelivery)
	router.GET("/:slug/deliveries/:revision/artifacts/:filename", h.DownloadAppDeliveryRevision)

	var archive bytes.Buffer
	files := tar.NewWriter(&archive)
	body := []byte("FROM scratch\n")
	if err := files.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	const plan = `{"format":1,"slug":"fresh-app","base_version":0,"summary":"fixture","contract":{"slug":"fresh-app","runtime":{"port":8080},"storage_mounts":[],"data_plan":{"strategy":"initialize","summary":"empty fixture","recovery":"nothing to restore"}},"files":[]}`
	rev := uuid.NewString()
	submit := func(endpoint string, fields map[string]string, saving bool) *httptest.ResponseRecorder {
		t.Helper()
		var requestBody bytes.Buffer
		multipartWriter := multipart.NewWriter(&requestBody)
		for k, v := range fields {
			if err := multipartWriter.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		artifacts := map[string][]byte{"app_tar": archive.Bytes(), "skill_zip": []byte("skill fixture")}
		if saving {
			artifacts["delivery_plan"] = []byte(plan)
		}
		for field, artifact := range artifacts {
			part, err := multipartWriter.CreateFormFile(field, field)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(artifact); err != nil {
				t.Fatal(err)
			}
		}
		if err := multipartWriter.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, endpoint, &requestBody)
		request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, request)
		return reply
	}
	save := submit("/deliveries", map[string]string{"slug": slug, "revision": rev}, true)
	if save.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", save.Code, save.Body)
	}
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", "/"+slug+"/deliveries/"+rev+"/artifacts/delivery.json", nil))
	if out.Code != http.StatusOK || out.Body.String() != plan {
		t.Fatalf("archived plan: %d %s", out.Code, out.Body)
	}

	pub := submit("/publish", map[string]string{"slug": slug, "name": "Fresh", "request_id": "test-request",
		"storage_mounts": "[]", "delivery_revision": rev, "delivery_plan": plan}, false)
	if pub.Code != http.StatusAccepted {
		t.Fatalf("publish: %d %s", pub.Code, pub.Body)
	}
	requestPath := "/" + slug + "/requests/test-request"
	wait := func(expected string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			reply := httptest.NewRecorder()
			router.ServeHTTP(reply, httptest.NewRequest("GET", requestPath, nil))
			var receipt map[string]any
			_ = json.Unmarshal(reply.Body.Bytes(), &receipt)
			if receipt["status"] == expected {
				return
			}
			if receipt["status"] == "failed" {
				t.Fatalf("delivery failed: %s", reply.Body)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", expected)
	}
	assess := func(phase string, status int) {
		t.Helper()
		content := fmt.Sprintf(`{"phase":%q,"outcome":"passed","report":"AI confirmed from recorded facts","evidence":["observed fixture"]}`, phase)
		req := httptest.NewRequest(http.MethodPost, requestPath+"/assessment", strings.NewReader(content))
		req.Header.Set("Content-Type", "application/json")
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, req)
		if reply.Code != status {
			t.Fatalf("%s assessment: %d %s", phase, reply.Code, reply.Body)
		}
	}
	wait("built")
	if starts.Load() != 0 {
		t.Fatal("candidate ran before build assessment")
	}
	assess("runtime", http.StatusConflict)
	assess("build", http.StatusAccepted)
	wait("runtime_ready")
	if starts.Load() != 1 {
		t.Fatal("approved image was not started exactly once")
	}
	before, err := h.storage.GetAppBySlug(t.Context(), slug)
	if err != nil || before.CurrentVersion != 0 {
		t.Fatalf("unassessed release was promoted: %+v %v", before, err)
	}
	assess("runtime", http.StatusOK)
	wait("running")
	assess("runtime", http.StatusOK)
	release, err := h.storage.GetActiveAppVersion(t.Context(), slug)
	if err != nil || release.Version != 1 || !strings.Contains(release.DeliveryAssessment, "observed fixture") {
		t.Fatalf("accepted release not persisted: %+v %v", release, err)
	}
}
