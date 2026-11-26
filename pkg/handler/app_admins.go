package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type updateAppAdminReq struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
}

func (h *Handler) AdminUpdateAppAdmin(c *gin.Context) {
	ctx := c.Request.Context()
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	app, err := h.storage.GetAppBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "应用不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var req updateAppAdminReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误"})
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.Action = strings.TrimSpace(strings.ToLower(req.Action))
	if req.UserID == "" || (req.Action != "add" && req.Action != "remove") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id 和 action(add|remove) 不能为空"})
		return
	}

	switch req.Action {
	case "add":
		user, uErr := h.storage.GetWeKnoraUserByID(ctx, req.UserID)
		if uErr != nil {
			if errors.Is(uErr, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": uErr.Error()})
			return
		}
		link, lErr := h.storage.GetActiveIdentityLinkByKnowledgeUserID(ctx, req.UserID)
		if lErr != nil || link == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "该员工尚未完成企微身份同步或未激活，暂无法授权"})
			return
		}
		ownerID := enterpriseAppOwner(link.OrganizationID, link.ImmutableSubject)
		if app.CreatedBy != "" && ownerID == app.CreatedBy {
			c.JSON(http.StatusOK, gin.H{
				"status": "ok",
				"admins": h.buildAppAdminSummaries(ctx, app),
			})
			return
		}
		if err := h.storage.AddAppAdmin(ctx, &model.AppAdmin{
			ID:        uuid.NewString(),
			AppSlug:   slug,
			OwnerID:   ownerID,
			UserID:    user.ID,
			Username:  user.Username,
			Email:     user.Email,
			GrantedBy: "admin",
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	case "remove":
		if app.CreatedBy != "" {
			if link, lErr := h.storage.GetActiveIdentityLinkByKnowledgeUserID(ctx, req.UserID); lErr == nil && link != nil {
				if enterpriseAppOwner(link.OrganizationID, link.ImmutableSubject) == app.CreatedBy {
					c.JSON(http.StatusBadRequest, gin.H{"error": "不能移除应用创建者"})
					return
				}
			}
		}
		if err := h.storage.RemoveAppAdminByUserID(ctx, slug, req.UserID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"admins": h.buildAppAdminSummaries(ctx, app),
	})
}

func (h *Handler) buildAppAdminSummaries(ctx context.Context, app *model.DeployedApp) []model.AppAdminSummary {
	if app == nil {
		return []model.AppAdminSummary{}
	}
	views := []AppDetailResponse{{Slug: app.Slug}}
	h.populateAppsAdmins(ctx, []model.DeployedApp{*app}, views)
	if views[0].Admins == nil {
		return []model.AppAdminSummary{}
	}
	return views[0].Admins
}

func (h *Handler) populateAppsAdmins(ctx context.Context, apps []model.DeployedApp, views []AppDetailResponse) {
	if len(apps) == 0 || len(apps) != len(views) {
		return
	}
	slugs := make([]string, len(apps))
	for i, app := range apps {
		slugs[i] = app.Slug
	}
	adminsBySlug, err := h.storage.ListAppAdminsBySlugs(ctx, slugs)
	if err != nil {
		adminsBySlug = map[string][]model.AppAdmin{}
	}
	links, _ := h.storage.ListActiveIdentityLinks(ctx)
	ownerToUserID := make(map[string]string, len(links))
	for _, l := range links {
		ownerToUserID[enterpriseAppOwner(l.OrganizationID, l.ImmutableSubject)] = l.KnowledgeUserID
	}

	userIDSet := make(map[string]bool)
	for _, app := range apps {
		if uid := ownerToUserID[app.CreatedBy]; uid != "" {
			userIDSet[uid] = true
		}
		for _, adm := range adminsBySlug[app.Slug] {
			if adm.UserID != "" {
				userIDSet[adm.UserID] = true
			}
		}
	}
	userIDs := make([]string, 0, len(userIDSet))
	for uid := range userIDSet {
		userIDs = append(userIDs, uid)
	}
	userMap, _ := h.storage.ListWeKnoraUsersByIDs(ctx, userIDs)

	for i, app := range apps {
		summaries := make([]model.AppAdminSummary, 0, 1+len(adminsBySlug[app.Slug]))
		if app.CreatedBy != "" {
			uid := ownerToUserID[app.CreatedBy]
			username := "创建者"
			email := ""
			if u, ok := userMap[uid]; ok {
				username = u.Username
				email = u.Email
			}
			summaries = append(summaries, model.AppAdminSummary{
				UserID:    uid,
				Username:  username,
				Email:     email,
				OwnerID:   app.CreatedBy,
				IsCreator: true,
			})
		}
		for _, adm := range adminsBySlug[app.Slug] {
			if adm.OwnerID == app.CreatedBy {
				continue
			}
			username := adm.Username
			email := adm.Email
			if u, ok := userMap[adm.UserID]; ok {
				username = u.Username
				email = u.Email
			}
			summaries = append(summaries, model.AppAdminSummary{
				UserID:    adm.UserID,
				Username:  username,
				Email:     email,
				OwnerID:   adm.OwnerID,
				IsCreator: false,
			})
		}
		views[i].Admins = summaries
	}
}
