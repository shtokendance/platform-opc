package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/workopc"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

type WorkOPCHandler struct {
	cfg     *config.Config
	storage *storage.Storage
	store   workopc.ObjectStore
}

func NewWorkOPCHandler(cfg *config.Config, st *storage.Storage, store workopc.ObjectStore) *WorkOPCHandler {
	return &WorkOPCHandler{
		cfg:     cfg,
		storage: st,
		store:   store,
	}
}

var (
	validWorkOPCChannels = map[string]bool{
		"stable":  true,
		"nightly": true,
	}
	validWorkOPCTargets = map[string]bool{
		"mac-arm64": true,
		"mac-x64":   true,
		"win-x64":   true,
	}
	validWorkOPCKinds = map[string]bool{
		"dmg":          true,
		"zip":          true,
		"zip_blockmap": true,
		"exe":          true,
		"exe_blockmap": true,
	}
)

func (h *WorkOPCHandler) publicBaseURL(c *gin.Context) string {
	if h.cfg != nil {
		if base := strings.TrimRight(strings.TrimSpace(h.cfg.WorkOPCReleases.PublicBaseURL), "/"); base != "" {
			return base
		}
		if base := strings.TrimRight(strings.TrimSpace(h.cfg.Server.ExternalBaseURL), "/"); base != "" {
			return base
		}
	}
	scheme := "https"
	if proto := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if c.Request != nil && c.Request.TLS == nil {
		scheme = "http"
	}
	host := ""
	if c.Request != nil {
		host = c.Request.Host
	}
	if host == "" {
		return "https://aiwork.benlai.com"
	}
	return scheme + "://" + host
}

func (h *WorkOPCHandler) adminActor(c *gin.Context) string {
	if claims, ok := adminauth.GetClaims(c); ok && claims.Subject != "" {
		return claims.Subject
	}
	return "admin"
}

func (h *WorkOPCHandler) recordAudit(c *gin.Context, releaseID, action string, detail interface{}) {
	raw, _ := json.Marshal(detail)
	_ = h.storage.InsertWorkOPCAuditLog(c.Request.Context(), &model.WorkOPCReleaseAuditLog{
		ReleaseID: releaseID,
		Actor:     h.adminActor(c),
		Action:    action,
		Detail:    string(raw),
	})
}

func inferArtifactTarget(filename string) string {
	lower := strings.ToLower(strings.TrimSpace(filename))
	switch {
	case strings.HasSuffix(lower, ".exe.blockmap"),
		strings.HasSuffix(lower, ".exe"),
		strings.Contains(lower, "win-x64"),
		strings.Contains(lower, "win32"),
		strings.Contains(lower, "windows"),
		strings.Contains(lower, "-win-"),
		strings.Contains(lower, "_win_"):
		return "win-x64"
	case strings.Contains(lower, "arm64"),
		strings.Contains(lower, "aarch64"),
		strings.Contains(lower, "apple-silicon"):
		return "mac-arm64"
	case strings.Contains(lower, "x64"),
		strings.Contains(lower, "amd64"),
		strings.Contains(lower, "intel"),
		strings.Contains(lower, "x86_64"):
		return "mac-x64"
	default:
		return ""
	}
}

func inferArtifactKind(filename string) string {
	lower := strings.ToLower(strings.TrimSpace(filename))
	switch {
	case strings.HasSuffix(lower, ".zip.blockmap"):
		return "zip_blockmap"
	case strings.HasSuffix(lower, ".exe.blockmap"):
		return "exe_blockmap"
	case strings.HasSuffix(lower, ".dmg"):
		return "dmg"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".exe"):
		return "exe"
	default:
		return ""
	}
}

func (h *WorkOPCHandler) AdminOverview(c *gin.Context) {
	ctx := c.Request.Context()
	stableRel, _ := h.storage.GetPublishedWorkOPCRelease(ctx, "stable")
	nightlyRel, _ := h.storage.GetPublishedWorkOPCRelease(ctx, "nightly")
	allReleases, err := h.storage.ListWorkOPCReleases(ctx, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	totalDownloads, _ := h.storage.GetWorkOPCTotalDownloads(ctx)
	base := h.publicBaseURL(c)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"storage_backend": h.store.BackendName(),
			"storage_bucket":  h.store.BucketName(),
			"total_releases":  len(allReleases),
			"total_downloads": totalDownloads,
			"active_stable":   stableRel,
			"active_nightly":  nightlyRel,
			"endpoints": gin.H{
				"download_mac_arm64": base + "/workopc/download/mac-arm64",
				"download_mac_x64":   base + "/workopc/download/mac-x64",
				"download_win_x64":   base + "/workopc/download/win-x64",
				"latest_json":        base + "/workopc/download/latest.json",
				"feed_mac_arm64":     base + "/workopc/update/feeds/mac-arm64/stable-mac.yml",
				"feed_mac_x64":       base + "/workopc/update/feeds/mac-x64/stable-mac.yml",
				"feed_win_x64":       base + "/workopc/update/feeds/win-x64/stable.yml",
				"policy":             base + "/workopc/update/policy",
			},
		},
	})
}

type workOPCInstallerLink struct {
	Target string `json:"target"`
	URL    string `json:"url"`
}

type workOPCReleaseListItem struct {
	model.WorkOPCRelease
	InstallerLinks []workOPCInstallerLink `json:"installer_links,omitempty"`
}

func workOPCReleaseCanDownload(release *model.WorkOPCRelease) bool {
	return release.Status == "published" || release.Status == "archived"
}

func workOPCInstallerKind(target string) string {
	if target == "win-x64" {
		return "exe"
	}
	return "dmg"
}

func workOPCInstallerFilename(version, target string) string {
	version = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '+':
			return r
		default:
			return '-'
		}
	}, version)
	return fmt.Sprintf("WorkOPC-%s-%s.%s", version, target, workOPCInstallerKind(target))
}

func (h *WorkOPCHandler) AdminListReleases(c *gin.Context) {
	channel := strings.TrimSpace(c.Query("channel"))
	if channel != "" && !validWorkOPCChannels[channel] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid channel"})
		return
	}
	releases, err := h.storage.ListWorkOPCReleases(c.Request.Context(), channel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	base := h.publicBaseURL(c)
	items := make([]workOPCReleaseListItem, 0, len(releases))
	for _, release := range releases {
		item := workOPCReleaseListItem{WorkOPCRelease: release}
		if workOPCReleaseCanDownload(&release) {
			for _, artifact := range release.Artifacts {
				if !validWorkOPCTargets[artifact.Target] || artifact.Kind != workOPCInstallerKind(artifact.Target) {
					continue
				}
				filename := workOPCInstallerFilename(release.Version, artifact.Target)
				item.InstallerLinks = append(item.InstallerLinks, workOPCInstallerLink{
					Target: artifact.Target,
					URL:    fmt.Sprintf("%s/workopc/installers/%s/%s/%s", base, url.PathEscape(release.ID), artifact.Target, url.PathEscape(filename)),
				})
			}
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": items})
}

type createWorkOPCReleaseReq struct {
	Version             string `json:"version"`
	Channel             string `json:"channel"`
	ReleaseNotes        string `json:"release_notes"`
	RolloutPercentage   *int   `json:"rollout_percentage"`
	IsMandatory         bool   `json:"is_mandatory"`
	MinSupportedVersion string `json:"min_supported_version"`
	CommitSHA           string `json:"commit_sha"`
}

func (h *WorkOPCHandler) AdminCreateRelease(c *gin.Context) {
	var req createWorkOPCReleaseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid request body"})
		return
	}
	req.Version = strings.TrimSpace(req.Version)
	req.Channel = strings.TrimSpace(strings.ToLower(req.Channel))
	if req.Channel == "" {
		req.Channel = "stable"
	}
	if req.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "version is required"})
		return
	}
	if !validWorkOPCChannels[req.Channel] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "channel must be stable or nightly"})
		return
	}
	rollout := 100
	if req.RolloutPercentage != nil {
		rollout = *req.RolloutPercentage
	}
	if rollout < 0 || rollout > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "rollout_percentage must be 0..100"})
		return
	}

	rel := &model.WorkOPCRelease{
		Version:             req.Version,
		Channel:             req.Channel,
		Status:              "draft",
		ReleaseNotes:        req.ReleaseNotes,
		RolloutPercentage:   rollout,
		IsMandatory:         req.IsMandatory,
		MinSupportedVersion: strings.TrimSpace(req.MinSupportedVersion),
		CommitSHA:           strings.TrimSpace(req.CommitSHA),
	}
	if err := h.storage.CreateWorkOPCRelease(c.Request.Context(), rel); err != nil {
		c.JSON(http.StatusConflict, gin.H{"code": -1, "message": fmt.Sprintf("create release failed: %v", err)})
		return
	}
	rel.Artifacts = []model.WorkOPCReleaseArtifact{}
	h.recordAudit(c, rel.ID, "release.create", gin.H{"version": rel.Version, "channel": rel.Channel})
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": rel})
}

func (h *WorkOPCHandler) AdminGetRelease(c *gin.Context) {
	rel, err := h.storage.GetWorkOPCRelease(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rel})
}

type updateWorkOPCReleaseReq struct {
	ReleaseNotes        *string `json:"release_notes"`
	RolloutPercentage   *int    `json:"rollout_percentage"`
	IsMandatory         *bool   `json:"is_mandatory"`
	MinSupportedVersion *string `json:"min_supported_version"`
	CommitSHA           *string `json:"commit_sha"`
}

func (h *WorkOPCHandler) AdminUpdateRelease(c *gin.Context) {
	var req updateWorkOPCReleaseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid request body"})
		return
	}
	updates := map[string]interface{}{}
	if req.ReleaseNotes != nil {
		updates["release_notes"] = *req.ReleaseNotes
	}
	if req.RolloutPercentage != nil {
		if *req.RolloutPercentage < 0 || *req.RolloutPercentage > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "rollout_percentage must be 0..100"})
			return
		}
		updates["rollout_percentage"] = *req.RolloutPercentage
	}
	if req.IsMandatory != nil {
		updates["is_mandatory"] = *req.IsMandatory
	}
	if req.MinSupportedVersion != nil {
		updates["min_supported_version"] = strings.TrimSpace(*req.MinSupportedVersion)
	}
	if req.CommitSHA != nil {
		updates["commit_sha"] = strings.TrimSpace(*req.CommitSHA)
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "no fields to update"})
		return
	}
	rel, err := h.storage.UpdateWorkOPCRelease(c.Request.Context(), c.Param("id"), updates)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	h.recordAudit(c, rel.ID, "release.update", updates)
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rel})
}

func (h *WorkOPCHandler) AdminUploadArtifact(c *gin.Context) {
	ctx := c.Request.Context()
	releaseID := c.Param("id")
	rel, err := h.storage.GetWorkOPCRelease(ctx, releaseID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	target := strings.TrimSpace(c.Query("target"))
	kind := strings.TrimSpace(c.Query("kind"))
	signed := c.Query("signed") == "true" || c.Query("signed") == "1"
	notarized := c.Query("notarized") == "true" || c.Query("notarized") == "1"

	var fileReader io.Reader
	var fileSize int64 = -1
	var filename string

	if strings.HasPrefix(strings.ToLower(c.ContentType()), "multipart/form-data") {
		mr, err := c.Request.MultipartReader()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("read multipart: %v", err)})
			return
		}
		var foundFile bool
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("read multipart part: %v", err)})
				return
			}
			formName := part.FormName()
			if formName == "file" {
				filename = filepath.Base(strings.TrimSpace(part.FileName()))
				fileReader = part
				foundFile = true
				break
			}
			valBytes, _ := io.ReadAll(io.LimitReader(part, 4096))
			val := strings.TrimSpace(string(valBytes))
			_ = part.Close()
			switch formName {
			case "target":
				target = val
			case "kind":
				kind = val
			case "signed":
				signed = val == "true" || val == "1"
			case "notarized":
				notarized = val == "true" || val == "1"
			}
		}
		if !foundFile || fileReader == nil || filename == "" || filename == "." {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "file part is required"})
			return
		}
	} else {
		filename = filepath.Base(strings.TrimSpace(c.Query("filename")))
		if filename == "" || filename == "." {
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "filename query param is required for raw upload"})
			return
		}
		fileReader = c.Request.Body
		if c.Request.ContentLength > 0 {
			fileSize = c.Request.ContentLength
		}
	}

	if target == "" {
		target = inferArtifactTarget(filename)
	}
	if !validWorkOPCTargets[target] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "target must be mac-arm64, mac-x64, or win-x64"})
		return
	}
	if kind == "" {
		kind = inferArtifactKind(filename)
	}
	if !validWorkOPCKinds[kind] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid artifact kind"})
		return
	}

	objectKey := workopc.BuildObjectKey(rel.Channel, rel.Version, target, filename)
	putRes, err := h.store.PutStream(ctx, objectKey, fileReader, fileSize, workopc.InferContentType(filename))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": fmt.Sprintf("store artifact: %v", err)})
		return
	}

	art := &model.WorkOPCReleaseArtifact{
		ReleaseID: rel.ID,
		Target:    target,
		Kind:      kind,
		Filename:  filename,
		ObjectKey: putRes.ObjectKey,
		SizeBytes: putRes.SizeBytes,
		SHA512:    putRes.SHA512,
		SHA256:    putRes.SHA256,
		Signed:    signed,
		Notarized: notarized,
	}
	if err := h.storage.UpsertWorkOPCArtifact(ctx, art); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": fmt.Sprintf("save artifact record: %v", err)})
		return
	}

	h.recordAudit(c, rel.ID, "artifact.upload", gin.H{
		"target":     target,
		"kind":       kind,
		"filename":   filename,
		"size_bytes": putRes.SizeBytes,
		"sha256":     putRes.SHA256,
	})
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": art})
}

func (h *WorkOPCHandler) AdminDeleteArtifact(c *gin.Context) {
	ctx := c.Request.Context()
	art, err := h.storage.DeleteWorkOPCArtifact(ctx, c.Param("id"), c.Param("artifactId"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "artifact not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	_ = h.store.DeleteObject(ctx, art.ObjectKey)
	h.recordAudit(c, c.Param("id"), "artifact.delete", gin.H{"artifact_id": art.ID, "filename": art.Filename})
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"deleted": true}})
}

func (h *WorkOPCHandler) AdminPublishRelease(c *gin.Context) {
	ctx := c.Request.Context()
	rel, err := h.storage.GetWorkOPCRelease(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	if len(rel.Artifacts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "upload at least one artifact before publishing"})
		return
	}
	published, err := h.storage.PublishWorkOPCRelease(ctx, rel.ID, h.adminActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	h.recordAudit(c, published.ID, "release.publish", gin.H{"version": published.Version, "channel": published.Channel})
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": published})
}

func (h *WorkOPCHandler) AdminRollbackRelease(c *gin.Context) {
	ctx := c.Request.Context()
	rel, err := h.storage.GetWorkOPCRelease(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	if len(rel.Artifacts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "cannot rollback to release without artifacts"})
		return
	}
	published, err := h.storage.PublishWorkOPCRelease(ctx, rel.ID, h.adminActor(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	h.recordAudit(c, published.ID, "release.rollback", gin.H{"version": published.Version, "channel": published.Channel})
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": published})
}

func (h *WorkOPCHandler) AdminYankRelease(c *gin.Context) {
	rel, err := h.storage.YankWorkOPCRelease(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	h.recordAudit(c, rel.ID, "release.yank", gin.H{"version": rel.Version, "channel": rel.Channel})
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": rel})
}

func (h *WorkOPCHandler) AdminDeleteRelease(c *gin.Context) {
	ctx := c.Request.Context()
	rel, err := h.storage.GetWorkOPCRelease(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "release not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	if rel.Status == "published" {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "cannot delete active published release; yank or publish another version first"})
		return
	}
	for _, art := range rel.Artifacts {
		_ = h.store.DeleteObject(ctx, art.ObjectKey)
	}
	if err := h.storage.DeleteWorkOPCRelease(ctx, rel.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	h.recordAudit(c, rel.ID, "release.delete", gin.H{"version": rel.Version, "channel": rel.Channel})
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"deleted": true}})
}

func (h *WorkOPCHandler) AdminListAuditLogs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	logs, err := h.storage.ListWorkOPCAuditLogs(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": logs})
}

func (h *WorkOPCHandler) PublicLatestJSON(c *gin.Context) {
	channel := strings.TrimSpace(strings.ToLower(c.Query("channel")))
	if channel == "" {
		channel = "stable"
	}
	if !validWorkOPCChannels[channel] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid channel"})
		return
	}
	rel, err := h.storage.GetPublishedWorkOPCRelease(c.Request.Context(), channel)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "no published release"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	base := h.publicBaseURL(c)
	targets := map[string]gin.H{}
	for _, t := range []string{"mac-arm64", "mac-x64", "win-x64"} {
		installerKind := workOPCInstallerKind(t)
		for _, art := range rel.Artifacts {
			if art.Target == t && art.Kind == installerKind {
				targets[t] = gin.H{
					"filename":   art.Filename,
					"size_bytes": art.SizeBytes,
					"sha256":     art.SHA256,
					"sha512":     art.SHA512,
					"signed":     art.Signed,
					"notarized":  art.Notarized,
					"url":        fmt.Sprintf("%s/workopc/download/%s", base, t),
				}
				break
			}
		}
	}

	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Cache-Control", "no-cache, max-age=0")
	c.JSON(http.StatusOK, gin.H{
		"version":               rel.Version,
		"channel":               rel.Channel,
		"release_notes":         rel.ReleaseNotes,
		"published_at":          rel.PublishedAt,
		"is_mandatory":          rel.IsMandatory,
		"min_supported_version": rel.MinSupportedVersion,
		"targets":               targets,
	})
}

func (h *WorkOPCHandler) PublicChangelog(c *gin.Context) {
	channel := strings.TrimSpace(strings.ToLower(c.Query("channel")))
	if channel != "" && !validWorkOPCChannels[channel] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid channel"})
		return
	}
	releases, err := h.storage.ListWorkOPCReleases(c.Request.Context(), channel)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	items := make([]gin.H, 0, len(releases))
	for _, rel := range releases {
		if rel.Status != "published" && rel.Status != "archived" {
			continue
		}
		items = append(items, gin.H{
			"version":       rel.Version,
			"channel":       rel.Channel,
			"status":        rel.Status,
			"release_notes": rel.ReleaseNotes,
			"published_at":  rel.PublishedAt,
		})
	}
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Cache-Control", "no-cache, max-age=0")
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": items})
}

func (h *WorkOPCHandler) PublicDownloadTarget(c *gin.Context) {
	ctx := c.Request.Context()
	target := strings.TrimSpace(c.Param("target"))
	if !validWorkOPCTargets[target] {
		c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "unsupported target"})
		return
	}
	channel := strings.TrimSpace(strings.ToLower(c.Query("channel")))
	if channel == "" {
		channel = "stable"
	}
	if !validWorkOPCChannels[channel] {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "invalid channel"})
		return
	}

	rel, err := h.storage.GetPublishedWorkOPCRelease(ctx, channel)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "no published release"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.Header("Cache-Control", "no-cache, max-age=0")
	h.serveWorkOPCInstaller(c, rel, target, "")
}

func (h *WorkOPCHandler) PublicDownloadReleaseInstaller(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	target := c.Param("target")
	if !validWorkOPCTargets[target] {
		c.Status(http.StatusNotFound)
		return
	}
	release, err := h.storage.GetWorkOPCRelease(c.Request.Context(), c.Param("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	filename := workOPCInstallerFilename(release.Version, target)
	if !workOPCReleaseCanDownload(release) || c.Param("filename") != filename {
		c.Status(http.StatusNotFound)
		return
	}
	h.serveWorkOPCInstaller(c, release, target, filename)
}

func (h *WorkOPCHandler) serveWorkOPCInstaller(c *gin.Context, rel *model.WorkOPCRelease, target, filename string) {
	ctx := c.Request.Context()
	kind := workOPCInstallerKind(target)
	for i := range rel.Artifacts {
		chosen := &rel.Artifacts[i]
		if chosen.Target != target || chosen.Kind != kind {
			continue
		}
		if filename == "" {
			filename = chosen.Filename
		}
		if c.Request.Method != http.MethodHead && c.GetHeader("Range") == "" {
			_ = h.storage.IncrementWorkOPCArtifactDownload(ctx, chosen.ID)
		}
		if err := h.store.ServeObject(ctx, c.Writer, c.Request, chosen.ObjectKey, filename, workopc.InferContentType(filename)); err != nil {
			if errors.Is(err, workopc.ErrArtifactNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "artifact object missing"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		}
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "installer artifact not available for target"})
}

type electronUpdaterFeed struct {
	Version           string                    `yaml:"version"`
	Files             []electronUpdaterFeedFile `yaml:"files"`
	Path              string                    `yaml:"path"`
	SHA512            string                    `yaml:"sha512"`
	ReleaseDate       string                    `yaml:"releaseDate"`
	StagingPercentage *int                      `yaml:"stagingPercentage,omitempty"`
}

type electronUpdaterFeedFile struct {
	URL    string `yaml:"url"`
	SHA512 string `yaml:"sha512"`
	Size   int64  `yaml:"size"`
}

func parseFeedFilename(target, filename string) (string, bool) {
	isMac := target == "mac-arm64" || target == "mac-x64"
	switch filename {
	case "stable-mac.yml":
		return "stable", isMac
	case "nightly-mac.yml":
		return "nightly", isMac
	case "latest-mac.yml":
		return "stable", isMac
	case "stable.yml":
		return "stable", !isMac
	case "nightly.yml":
		return "nightly", !isMac
	case "latest.yml":
		return "stable", !isMac
	default:
		return "", false
	}
}

func (h *WorkOPCHandler) PublicUpdateFeed(c *gin.Context) {
	ctx := c.Request.Context()
	target := strings.TrimSpace(c.Param("target"))
	filename := strings.TrimSpace(c.Param("filename"))
	if !validWorkOPCTargets[target] {
		c.Status(http.StatusNotFound)
		return
	}
	channel, ok := parseFeedFilename(target, filename)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}

	rel, err := h.storage.GetPublishedWorkOPCRelease(ctx, channel)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	isMac := target == "mac-arm64" || target == "mac-x64"
	wantKind := "exe"
	if isMac {
		wantKind = "zip"
	}
	var updateArt *model.WorkOPCReleaseArtifact
	for i := range rel.Artifacts {
		if rel.Artifacts[i].Target == target && rel.Artifacts[i].Kind == wantKind {
			updateArt = &rel.Artifacts[i]
			break
		}
	}
	if updateArt == nil {
		c.Status(http.StatusNotFound)
		return
	}

	relURL := fmt.Sprintf("../../bin/%s/%s/%s", target, channel, updateArt.Filename)
	publishedAt := rel.UpdatedAt
	if rel.PublishedAt != nil {
		publishedAt = *rel.PublishedAt
	}
	feed := electronUpdaterFeed{
		Version: rel.Version,
		Files: []electronUpdaterFeedFile{
			{
				URL:    relURL,
				SHA512: updateArt.SHA512,
				Size:   updateArt.SizeBytes,
			},
		},
		Path:        relURL,
		SHA512:      updateArt.SHA512,
		ReleaseDate: publishedAt.UTC().Format(time.RFC3339Nano),
	}
	if rel.RolloutPercentage >= 0 && rel.RolloutPercentage < 100 {
		pct := rel.RolloutPercentage
		feed.StagingPercentage = &pct
	}

	out, err := yaml.Marshal(&feed)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "no-cache, max-age=0")
	c.Data(http.StatusOK, "text/yaml; charset=utf-8", out)
}

func (h *WorkOPCHandler) PublicUpdateBinary(c *gin.Context) {
	ctx := c.Request.Context()
	target := strings.TrimSpace(c.Param("target"))
	channel := strings.TrimSpace(strings.ToLower(c.Param("channel")))
	filename := filepath.Base(strings.TrimSpace(c.Param("filename")))
	if rest := strings.Trim(strings.TrimSpace(c.Param("filepath")), "/"); rest != "" {
		parts := strings.Split(rest, "/")
		switch len(parts) {
		case 1:
			channel = ""
			filename = filepath.Base(strings.TrimSpace(parts[0]))
		case 2:
			channel = strings.TrimSpace(strings.ToLower(parts[0]))
			filename = filepath.Base(strings.TrimSpace(parts[1]))
		default:
			c.Status(http.StatusNotFound)
			return
		}
	}
	if !validWorkOPCTargets[target] || filename == "" || filename == "." {
		c.Status(http.StatusNotFound)
		return
	}
	if channel != "" && !validWorkOPCChannels[channel] {
		c.Status(http.StatusNotFound)
		return
	}

	art, err := h.storage.FindWorkOPCArtifactForDownload(ctx, channel, target, filename)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	if c.Request.Method != http.MethodHead && c.GetHeader("Range") == "" && !strings.HasSuffix(strings.ToLower(filename), ".blockmap") {
		_ = h.storage.IncrementWorkOPCArtifactDownload(ctx, art.ID)
	}
	if err := h.store.ServeObject(ctx, c.Writer, c.Request, art.ObjectKey, art.Filename, workopc.InferContentType(art.Filename)); err != nil {
		if errors.Is(err, workopc.ErrArtifactNotFound) {
			c.Status(http.StatusNotFound)
			return
		}
		c.Status(http.StatusInternalServerError)
	}
}

func (h *WorkOPCHandler) PublicUpdatePolicy(c *gin.Context) {
	ctx := c.Request.Context()
	channel := strings.TrimSpace(strings.ToLower(firstNonEmpty(
		c.GetHeader("x-client-update-channel"),
		c.Query("channel"),
		"stable",
	)))
	if !validWorkOPCChannels[channel] {
		channel = "stable"
	}
	clientVersion := strings.TrimSpace(firstNonEmpty(
		c.GetHeader("x-client-version"),
		c.Query("version"),
	))
	platform := strings.TrimSpace(strings.ToLower(firstNonEmpty(
		c.GetHeader("x-client-platform"),
		c.Query("platform"),
	)))
	arch := strings.TrimSpace(strings.ToLower(firstNonEmpty(
		c.GetHeader("x-client-arch"),
		c.Query("arch"),
	)))

	c.Header("Cache-Control", "no-store")
	rel, err := h.storage.GetPublishedWorkOPCRelease(ctx, channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"biz_code": 0,
				"biz_data": nil,
			},
		})
		return
	}

	minVer := strings.TrimSpace(rel.MinSupportedVersion)
	if minVer == "" && rel.IsMandatory {
		minVer = rel.Version
	}
	shouldBlock := false
	if clientVersion != "" && minVer != "" && (rel.IsMandatory || rel.MinSupportedVersion != "") {
		if compareSemver(clientVersion, minVer) < 0 {
			shouldBlock = true
		}
	}

	if !shouldBlock {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"biz_code": 0,
				"biz_data": nil,
			},
		})
		return
	}

	target := resolveTargetFromPlatformArch(platform, arch)
	detail := strings.TrimSpace(rel.ReleaseNotes)
	if detail == "" {
		detail = fmt.Sprintf("当前版本 (%s) 已停止支持，请升级到 WorkOPC %s 后继续使用。", clientVersion, rel.Version)
	}
	c.JSON(http.StatusOK, gin.H{
		"code": 40005,
		"data": gin.H{
			"show_content": gin.H{
				"title":  fmt.Sprintf("需要更新到 WorkOPC %s", rel.Version),
				"detail": detail,
			},
			"desktop_app_link": fmt.Sprintf("%s/workopc/download/%s", h.publicBaseURL(c), target),
		},
	})
}

func resolveTargetFromPlatformArch(platform, arch string) string {
	if strings.HasPrefix(platform, "win") {
		return "win-x64"
	}
	if arch == "x64" || arch == "amd64" {
		return "mac-x64"
	}
	return "mac-arm64"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func compareSemver(a, b string) int {
	pa := parseSemverParts(a)
	pb := parseSemverParts(b)
	for i := 0; i < 3; i++ {
		if pa.nums[i] != pb.nums[i] {
			if pa.nums[i] < pb.nums[i] {
				return -1
			}
			return 1
		}
	}
	if pa.pre == pb.pre {
		return 0
	}
	if pa.pre == "" {
		return 1
	}
	if pb.pre == "" {
		return -1
	}
	if pa.pre < pb.pre {
		return -1
	}
	return 1
}

type semverParts struct {
	nums [3]int
	pre  string
}

func parseSemverParts(raw string) semverParts {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		s = s[:idx]
	}
	var out semverParts
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		out.pre = s[idx+1:]
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n, _ := strconv.Atoi(parts[i])
		out.nums[i] = n
	}
	return out
}
