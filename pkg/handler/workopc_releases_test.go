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
	r.GET("/workopc/update/changelog", h.PublicChangelog)
	r.GET("/workopc/download/:target", h.PublicDownloadTarget)
	r.HEAD("/workopc/download/:target", h.PublicDownloadTarget)
	r.GET("/workopc/update/feeds/:target/:filename", h.PublicUpdateFeed)
	r.GET("/workopc/update/bin/:target/*filepath", h.PublicUpdateBinary)
	r.HEAD("/workopc/update/bin/:target/*filepath", h.PublicUpdateBinary)
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
	if latestRec.Code != http.StatusOK || !strings.Contains(latestRec.Body.String(), `"version":"0.2.4"`) || latestRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("latest.json failed: %d %s", latestRec.Code, latestRec.Body.String())
	}

	changelogRec := httptest.NewRecorder()
	r.ServeHTTP(changelogRec, httptest.NewRequest(http.MethodGet, "/workopc/update/changelog", nil))
	if changelogRec.Code != http.StatusOK || !strings.Contains(changelogRec.Body.String(), `"New release"`) || changelogRec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("changelog failed: %d %s", changelogRec.Code, changelogRec.Body.String())
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

	legacyBinRec := httptest.NewRecorder()
	r.ServeHTTP(legacyBinRec, httptest.NewRequest(http.MethodGet, "/workopc/update/bin/mac-arm64/WorkOPC-0.2.4-mac-arm64.zip", nil))
	if legacyBinRec.Code != http.StatusOK || !bytes.Equal(legacyBinRec.Body.Bytes(), zipBytes) {
		t.Fatalf("bin without channel failed: %d %q", legacyBinRec.Code, legacyBinRec.Body.String())
	}

	deepBinRec := httptest.NewRecorder()
	r.ServeHTTP(deepBinRec, httptest.NewRequest(http.MethodGet, "/workopc/update/bin/mac-arm64/stable/extra/WorkOPC-0.2.4-mac-arm64.zip", nil))
	if deepBinRec.Code != http.StatusNotFound {
		t.Fatalf("bin with extra segments should 404, got: %d", deepBinRec.Code)
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

func TestWorkOPCInferArtifactTarget(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"WorkOPC-0.2.4-mac-arm64.dmg", "mac-arm64"},
		{"WorkOPC-0.2.4-mac-arm64.zip", "mac-arm64"},
		{"WorkOPC-0.2.4-mac-arm64.zip.blockmap", "mac-arm64"},
		{"WorkOPC-0.2.4-aarch64.dmg", "mac-arm64"},
		{"WorkOPC-0.2.4-mac-x64.dmg", "mac-x64"},
		{"WorkOPC-0.2.4-intel.zip", "mac-x64"},
		{"WorkOPC-0.2.4-win-x64.exe", "win-x64"},
		{"WorkOPC-0.2.4-win-x64.exe.blockmap", "win-x64"},
		{"WorkOPC-Setup-0.2.4.exe", "win-x64"},
		{"WorkOPC-0.2.4.dmg", ""},
	}
	for _, tc := range cases {
		if got := inferArtifactTarget(tc.filename); got != tc.want {
			t.Fatalf("inferArtifactTarget(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}

func TestWorkOPCUploadAutoInfersTarget(t *testing.T) {
	r, _ := setupWorkOPCTestRouter(t)

	createBody := `{"version":"0.2.5","channel":"stable"}`
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

	uploadTestArtifact(t, r, releaseID, "", "", "WorkOPC-0.2.5-mac-arm64.dmg", []byte("dmg-data"))
	uploadTestArtifact(t, r, releaseID, "", "", "WorkOPC-0.2.5-mac-x64.zip", []byte("zip-data"))
	uploadTestArtifact(t, r, releaseID, "", "", "WorkOPC-0.2.5-win-x64.exe", []byte("exe-data"))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "WorkOPC-0.2.5.dmg")
	_, _ = fw.Write([]byte("ambiguous"))
	_ = mw.Close()
	badReq := httptest.NewRequest(http.MethodPost, "/admin/api/workopc/releases/"+releaseID+"/artifacts", &buf)
	badReq.Header.Set("Content-Type", mw.FormDataContentType())
	badRec := httptest.NewRecorder()
	r.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ambiguous filename without target, got %d %s", badRec.Code, badRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	r.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/admin/api/workopc/releases/"+releaseID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get release failed: %d", getRec.Code)
	}
	var relResp struct {
		Data struct {
			Artifacts []model.WorkOPCReleaseArtifact `json:"artifacts"`
		} `json:"data"`
	}
	_ = json.Unmarshal(getRec.Body.Bytes(), &relResp)
	if len(relResp.Data.Artifacts) != 3 {
		t.Fatalf("expected 3 artifacts, got %d", len(relResp.Data.Artifacts))
	}
	targets := map[string]string{}
	for _, a := range relResp.Data.Artifacts {
		targets[a.Filename] = a.Target + ":" + a.Kind
	}
	if targets["WorkOPC-0.2.5-mac-arm64.dmg"] != "mac-arm64:dmg" ||
		targets["WorkOPC-0.2.5-mac-x64.zip"] != "mac-x64:zip" ||
		targets["WorkOPC-0.2.5-win-x64.exe"] != "win-x64:exe" {
		t.Fatalf("unexpected inferred artifacts: %+v", targets)
	}
}

func TestWorkOPCFeedFallsBackPerTargetWhenLatestLacksPlatform(t *testing.T) {
	r, _ := setupWorkOPCTestRouter(t)

	createRelease := func(version string) string {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/workopc/releases", strings.NewReader(fmt.Sprintf(`{"version":%q,"channel":"stable"}`, version)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s failed: %d %s", version, rec.Code, rec.Body.String())
		}
		var resp struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Data.ID
	}
	publish := func(id string) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/workopc/releases/"+id+"/publish", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("publish %s failed: %d %s", id, rec.Code, rec.Body.String())
		}
	}

	relOld := createRelease("0.2.3-alpha.2")
	uploadTestArtifact(t, r, relOld, "mac-arm64", "dmg", "WorkOPC-0.2.3-alpha.2-mac-arm64.dmg", []byte("old-dmg"))
	uploadTestArtifact(t, r, relOld, "mac-arm64", "zip", "WorkOPC-0.2.3-alpha.2-mac-arm64.zip", []byte("old-zip"))
	uploadTestArtifact(t, r, relOld, "win-x64", "exe", "WorkOPC-0.2.3-alpha.2-win-x64.exe", []byte("old-exe"))
	publish(relOld)

	relNew := createRelease("0.2.5")
	uploadTestArtifact(t, r, relNew, "mac-arm64", "dmg", "WorkOPC-0.2.5-mac-arm64.dmg", []byte("new-dmg"))
	uploadTestArtifact(t, r, relNew, "mac-arm64", "zip", "WorkOPC-0.2.5-mac-arm64.zip", []byte("new-zip"))
	publish(relNew)

	macFeedRec := httptest.NewRecorder()
	r.ServeHTTP(macFeedRec, httptest.NewRequest(http.MethodGet, "/workopc/update/feeds/mac-arm64/stable-mac.yml", nil))
	if macFeedRec.Code != http.StatusOK || !strings.Contains(macFeedRec.Body.String(), "version: 0.2.5") {
		t.Fatalf("expected mac feed 0.2.5, got %d %s", macFeedRec.Code, macFeedRec.Body.String())
	}

	winFeedRec := httptest.NewRecorder()
	r.ServeHTTP(winFeedRec, httptest.NewRequest(http.MethodGet, "/workopc/update/feeds/win-x64/stable.yml", nil))
	if winFeedRec.Code != http.StatusOK || !strings.Contains(winFeedRec.Body.String(), "version: 0.2.3-alpha.2") {
		t.Fatalf("expected win feed fallback to 0.2.3-alpha.2, got %d %s", winFeedRec.Code, winFeedRec.Body.String())
	}

	winDlRec := httptest.NewRecorder()
	r.ServeHTTP(winDlRec, httptest.NewRequest(http.MethodGet, "/workopc/download/win-x64", nil))
	if winDlRec.Code != http.StatusOK || winDlRec.Body.String() != "old-exe" {
		t.Fatalf("expected win download fallback to old-exe, got %d %q", winDlRec.Code, winDlRec.Body.String())
	}
}
