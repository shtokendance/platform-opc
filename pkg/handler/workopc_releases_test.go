package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/workopc"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupWorkOPCTestRouter(t *testing.T) (*gin.Engine, *storage.Storage) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsnMain := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	dsnState := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	mainDB, err := gorm.Open(sqlite.Open(dsnMain), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	stateDB, err := gorm.Open(sqlite.Open(dsnState), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	st := storage.NewStorageWithDBs(mainDB, stateDB)
	if err := stateDB.AutoMigrate(&model.WorkOPCRelease{}, &model.WorkOPCReleaseArtifact{}, &model.WorkOPCReleaseAuditLog{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	objStore, err := workopc.NewStore(workopc.StoreConfig{LocalDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{ExternalBaseURL: "https://aiwork.benlai.com"},
	}
	h := NewWorkOPCHandler(cfg, st, objStore)
	r := gin.New()

	r.GET("/workopc/download/latest.json", h.PublicLatestJSON)
	r.GET("/workopc/download/:target", h.PublicDownloadTarget)
	r.HEAD("/workopc/download/:target", h.PublicDownloadTarget)
	r.GET("/workopc/update/feeds/:target/:filename", h.PublicUpdateFeed)
	r.GET("/workopc/update/bin/:target/:channel/:filename", h.PublicUpdateBinary)
	r.HEAD("/workopc/update/bin/:target/:channel/:filename", h.PublicUpdateBinary)
	r.GET("/workopc/update/policy", h.PublicUpdatePolicy)
	r.GET("/workopc/update/api/v0/check_client_update", h.PublicUpdatePolicy)

	admin := r.Group("/admin/api/workopc")
	admin.GET("/overview", h.AdminOverview)
	admin.GET("/releases", h.AdminListReleases)
	admin.POST("/releases", h.AdminCreateRelease)
	admin.GET("/releases/:id", h.AdminGetRelease)
	admin.PUT("/releases/:id", h.AdminUpdateRelease)
	admin.DELETE("/releases/:id", h.AdminDeleteRelease)
	admin.POST("/releases/:id/artifacts", h.AdminUploadArtifact)
	admin.DELETE("/releases/:id/artifacts/:artifactId", h.AdminDeleteArtifact)
	admin.POST("/releases/:id/publish", h.AdminPublishRelease)
	admin.POST("/releases/:id/rollback", h.AdminRollbackRelease)
	admin.POST("/releases/:id/yank", h.AdminYankRelease)
	admin.GET("/audit", h.AdminListAuditLogs)

	return r, st
}

func uploadTestArtifact(t *testing.T, r *gin.Engine, releaseID, target, kind, filename string, content []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("target", target)
	_ = mw.WriteField("kind", kind)
	_ = mw.WriteField("signed", "true")
	_ = mw.WriteField("notarized", "true")
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/admin/api/workopc/releases/"+releaseID+"/artifacts", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s failed: code=%d body=%s", filename, rec.Code, rec.Body.String())
	}
}

func TestWorkOPCEndToEndReleaseAndAutoUpdate(t *testing.T) {
	r, _ := setupWorkOPCTestRouter(t)

	createBody := `{"version":"0.2.4","channel":"stable","release_notes":"New release","rollout_percentage":50,"is_mandatory":true,"min_supported_version":"0.2.3"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/workopc/releases", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create release failed: %d %s", rec.Code, rec.Body.String())
	}
	var createResp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &createResp)
	releaseID := createResp.Data.ID

	dmgBytes := []byte("fake-dmg-installer-binary-content")
	zipBytes := []byte("fake-mac-update-zip-binary-content-0123456789")
	exeBytes := []byte("fake-win-nsis-installer-binary-content")

	uploadTestArtifact(t, r, releaseID, "mac-arm64", "dmg", "WorkOPC-0.2.4-mac-arm64.dmg", dmgBytes)
	uploadTestArtifact(t, r, releaseID, "mac-arm64", "zip", "WorkOPC-0.2.4-mac-arm64.zip", zipBytes)
	uploadTestArtifact(t, r, releaseID, "win-x64", "exe", "WorkOPC-0.2.4-win-x64.exe", exeBytes)

	pubReq := httptest.NewRequest(http.MethodPost, "/admin/api/workopc/releases/"+releaseID+"/publish", nil)
	pubRec := httptest.NewRecorder()
	r.ServeHTTP(pubRec, pubReq)
	if pubRec.Code != http.StatusOK {
		t.Fatalf("publish failed: %d %s", pubRec.Code, pubRec.Body.String())
	}

	latestRec := httptest.NewRecorder()
	r.ServeHTTP(latestRec, httptest.NewRequest(http.MethodGet, "/workopc/download/latest.json", nil))
	if latestRec.Code != http.StatusOK || !strings.Contains(latestRec.Body.String(), `"version":"0.2.4"`) {
		t.Fatalf("latest.json failed: %d %s", latestRec.Code, latestRec.Body.String())
	}

	dlRec := httptest.NewRecorder()
	r.ServeHTTP(dlRec, httptest.NewRequest(http.MethodGet, "/workopc/download/mac-arm64", nil))
	if dlRec.Code != http.StatusOK || !bytes.Equal(dlRec.Body.Bytes(), dmgBytes) {
		t.Fatalf("download mac-arm64 failed: %d %q", dlRec.Code, dlRec.Body.String())
	}

	feedRec := httptest.NewRecorder()
	r.ServeHTTP(feedRec, httptest.NewRequest(http.MethodGet, "/workopc/update/feeds/mac-arm64/stable-mac.yml", nil))
	if feedRec.Code != http.StatusOK {
		t.Fatalf("feed failed: %d %s", feedRec.Code, feedRec.Body.String())
	}
	if !strings.Contains(feedRec.Header().Get("Cache-Control"), "no-cache") {
		t.Fatalf("feed missing no-cache header: %s", feedRec.Header().Get("Cache-Control"))
	}
	var parsedFeed electronUpdaterFeed
	if err := yaml.Unmarshal(feedRec.Body.Bytes(), &parsedFeed); err != nil {
		t.Fatalf("unmarshal feed yaml: %v", err)
	}
	if parsedFeed.Version != "0.2.4" || parsedFeed.Path != "../../bin/mac-arm64/stable/WorkOPC-0.2.4-mac-arm64.zip" || parsedFeed.StagingPercentage == nil || *parsedFeed.StagingPercentage != 50 {
		t.Fatalf("unexpected feed: %+v", parsedFeed)
	}

	binReq := httptest.NewRequest(http.MethodGet, "/workopc/update/bin/mac-arm64/stable/WorkOPC-0.2.4-mac-arm64.zip", nil)
	binReq.Header.Set("Range", "bytes=0-7")
	binRec := httptest.NewRecorder()
	r.ServeHTTP(binRec, binReq)
	if binRec.Code != http.StatusPartialContent || binRec.Body.String() != "fake-mac" {
		t.Fatalf("bin range failed: %d %q", binRec.Code, binRec.Body.String())
	}
	if !strings.Contains(binRec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("bin missing immutable cache header")
	}

	oldPolicyReq := httptest.NewRequest(http.MethodGet, "/workopc/update/api/v0/check_client_update?scenario=launch", nil)
	oldPolicyReq.Header.Set("x-client-version", "0.2.2")
	oldPolicyReq.Header.Set("x-client-platform", "darwin")
	oldPolicyReq.Header.Set("x-client-arch", "arm64")
	oldPolicyReq.Header.Set("x-client-update-channel", "stable")
	oldPolicyRec := httptest.NewRecorder()
	r.ServeHTTP(oldPolicyRec, oldPolicyReq)
	if oldPolicyRec.Code != http.StatusOK || !strings.Contains(oldPolicyRec.Body.String(), `"code":40005`) || !strings.Contains(oldPolicyRec.Body.String(), "https://aiwork.benlai.com/workopc/download/mac-arm64") {
		t.Fatalf("expected mandatory block for 0.2.2, got: %d %s", oldPolicyRec.Code, oldPolicyRec.Body.String())
	}

	newPolicyReq := httptest.NewRequest(http.MethodGet, "/workopc/update/policy", nil)
	newPolicyReq.Header.Set("x-client-version", "0.2.4")
	newPolicyReq.Header.Set("x-client-update-channel", "stable")
	newPolicyRec := httptest.NewRecorder()
	r.ServeHTTP(newPolicyRec, newPolicyReq)
	if newPolicyRec.Code != http.StatusOK || !strings.Contains(newPolicyRec.Body.String(), `"biz_code":0`) {
		t.Fatalf("expected no-force for 0.2.4, got: %d %s", newPolicyRec.Code, newPolicyRec.Body.String())
	}
}
