package handler

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// An application owns its prepared revisions even before it has a deployment.
// The artifact prefix includes the content digest so concurrent saves cannot
// overwrite an accepted revision.
type appDeliveryRevision model.AppDeliveryRevision

func (r appDeliveryRevision) artifactPrefix() string {
	digest := sha256.Sum256([]byte(r.AppSHA + r.SkillSHA + r.PlanSHA))
	return fmt.Sprintf("delivery-revisions/%x/%s/%s/%x", sha256.Sum256([]byte(r.Owner)), r.Slug, r.ID, digest)
}

func readDeliveryUpload(c *gin.Context, field string, limit int64) ([]byte, error) {
	header, err := c.FormFile(field)
	if err != nil || header.Size == 0 || header.Size > limit {
		return nil, fmt.Errorf("missing or oversized %s", field)
	}
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("could not read %s", field)
	}
	return data, nil
}

func (h *Handler) SaveAppDeliveryRevision(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<20)
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	ctx := c.Request.Context()
	slug := strings.TrimSpace(c.PostForm("slug"))
	id, err := uuid.Parse(c.PostForm("revision"))
	owner := c.GetString(appOwnerKey)
	if owner == "" || err != nil || id == uuid.Nil || !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid application or delivery revision"})
		return
	}
	if !h.checkAppOwner(c, slug, true) {
		return
	}
	plan, err := readDeliveryUpload(c, "delivery_plan", 2<<20)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	app, err := readDeliveryUpload(c, "app_tar", 240<<20)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	skill, err := readDeliveryUpload(c, "skill_zip", 8<<20)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var header struct {
		BaseVersion int `json:"base_version"`
		Contract    struct {
			Runtime struct {
				Port int `json:"port"`
			} `json:"runtime"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(plan, &header); err != nil || header.BaseVersion < 0 ||
		header.Contract.Runtime.Port < 1 || header.Contract.Runtime.Port > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid delivery plan header"})
		return
	}
	if err := validateDeliveryPlan(plan, slug, header.Contract.Runtime.Port, header.BaseVersion, app); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, err := readDeliveryDataPlan(plan); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	revision := appDeliveryRevision{
		Owner: owner, Slug: slug, ID: id, BaseVersion: header.BaseVersion,
		AppSHA: fmt.Sprintf("%x", sha256.Sum256(app)), SkillSHA: fmt.Sprintf("%x", sha256.Sum256(skill)),
		PlanSHA: fmt.Sprintf("%x", sha256.Sum256(plan)),
	}
	db := h.storage.DeployDB().WithContext(ctx)
	result := db.Where("owner = ? AND slug = ? AND id = ?", owner, slug, id)
	var saved appDeliveryRevision
	if err := result.First(&saved).Error; err == nil {
		if saved.AppSHA != revision.AppSHA || saved.SkillSHA != revision.SkillSHA || saved.PlanSHA != revision.PlanSHA {
			c.JSON(http.StatusConflict, gin.H{"error": "delivery revision already has different artifacts"})
			return
		}
		c.JSON(http.StatusOK, saved)
		return
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery revision could not be read"})
		return
	}
	store := h.resolveArtifactStore(ctx)
	prefix := revision.artifactPrefix()
	for _, file := range []struct {
		name, contentType string
		data              []byte
	}{
		{"delivery.json", "application/json", plan},
		{"app.tar", "application/x-tar", app},
		{"skill.zip", "application/zip", skill},
	} {
		if err := store.PutObject(ctx, prefix+"/"+file.name, file.data, file.contentType); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery revision artifacts could not be stored"})
			return
		}
	}
	if err := db.Create(&revision).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "delivery revision changed while saving, read its receipt before retrying"})
		return
	}
	c.JSON(http.StatusCreated, revision)
}

func (h *Handler) ListAppDeliveryRevisions(c *gin.Context) {
	db := h.storage.DeployDB().WithContext(c.Request.Context())
	revisions := []appDeliveryRevision{}
	if err := db.Where("owner = ? AND slug = ?", c.GetString(appOwnerKey), c.Param("slug")).
		Order("created_at DESC").Limit(50).Find(&revisions).Error; err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery revisions could not be read"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revisions": revisions})
}

func (h *Handler) DownloadAppDeliveryRevision(c *gin.Context) {
	id, err := uuid.Parse(c.Param("revision"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid revision"})
		return
	}
	ctx := c.Request.Context()
	db := h.storage.DeployDB().WithContext(ctx)
	var revision appDeliveryRevision
	if err := db.Where("owner = ? AND slug = ? AND id = ?", c.GetString(appOwnerKey), c.Param("slug"), id).First(&revision).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "delivery revision not found"})
		return
	}
	name := c.Param("filename")
	expected := map[string]string{"delivery.json": revision.PlanSHA, "app.tar": revision.AppSHA, "skill.zip": revision.SkillSHA}[name]
	if expected == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "delivery artifact not found"})
		return
	}
	data, contentType, err := h.resolveArtifactStore(ctx).GetObject(ctx, revision.artifactPrefix()+"/"+name)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != expected {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "delivery artifact could not be verified"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Artifact-SHA256", expected)
	c.Data(http.StatusOK, contentType, data)
}
