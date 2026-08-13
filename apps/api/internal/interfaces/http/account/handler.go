package interfaceshttpaccount

import (
	applicationaccount "GCFeed/internal/application/account"
	domainaccount "GCFeed/internal/domain/account"
	interfaceshttpmiddleware "GCFeed/internal/interfaces/http/middleware"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// RefreshTokenCookieName 是 refresh token Cookie 名,HttpOnly 防 JS 读取。
	RefreshTokenCookieName = "gcfeed_refresh_token"
	// refreshCookiePath 限定在 sessions 前缀携带,减少其余请求的传输暴露。
	refreshCookiePath = "/api/sessions"
)

type Handler struct {
	service       *applicationaccount.Service
	secureCookie  bool
	refreshMaxAge int
}

// New 注入账号应用服务，Handler 只处理 HTTP 输入输出。
func New(service *applicationaccount.Service, secureCookie bool, refreshTTL time.Duration) *Handler {
	return &Handler{
		service:       service,
		secureCookie:  secureCookie,
		refreshMaxAge: int(refreshTTL.Seconds()),
	}
}

// Register 处理用户注册请求，成功后返回新用户资料。
func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// 具体注册规则在应用层和领域层执行，HTTP 层只传递请求字段。
	profile, err := h.service.Register(c.Request.Context(), req.Account, req.Password, req.Nickname)
	if err != nil {
		if isBadRequestError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, domainaccount.ErrAccountAlreadyExists) {
			c.JSON(http.StatusConflict, gin.H{"error": "account already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
		return
	}

	c.JSON(http.StatusCreated, profileResponse(profile))
}

// Login 处理账号密码登录，成功后返回 Bearer token。
func (h *Handler) Login(c *gin.Context) {
	var req LoginByPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// 登录失败统一映射为 401，避免暴露账号是否存在。
	token, err := h.service.Login(c.Request.Context(), req.Account, req.Password)
	if err != nil {
		if isBadRequestError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, domainaccount.ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
		return
	}

	h.setRefreshCookie(c, token.RefreshToken)
	c.JSON(http.StatusOK, tokenResponse{
		AccessToken:      token.AccessToken,
		TokenType:        token.TokenType,
		ExpiresInSeconds: token.ExpiresInSeconds,
	})
}

// Refresh 用 Cookie 里的 refresh token 换新 token 对:校验 + 轮换都由应用层完成。
// 任何失败统一 401 且清 Cookie,不区分错误枚举,防止探测有效 token。
func (h *Handler) Refresh(c *gin.Context) {
	plainToken, _ := c.Cookie(RefreshTokenCookieName)
	token, err := h.service.Refresh(c.Request.Context(), plainToken)
	if err != nil {
		h.clearRefreshCookie(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "refresh token invalid"})
		return
	}

	h.setRefreshCookie(c, token.RefreshToken)
	c.JSON(http.StatusOK, tokenResponse{
		AccessToken:      token.AccessToken,
		TokenType:        token.TokenType,
		ExpiresInSeconds: token.ExpiresInSeconds,
	})
}

// Logout 只凭 refresh Cookie 撤销当前会话,不依赖 access token——
// access 过期后仍能真正登出;没有 Cookie 也返回 204 保持幂等。
func (h *Handler) Logout(c *gin.Context) {
	plainToken, _ := c.Cookie(RefreshTokenCookieName)
	_ = h.service.Logout(c.Request.Context(), plainToken)
	h.clearRefreshCookie(c)
	c.Status(http.StatusNoContent)
}

// setRefreshCookie 手写 Set-Cookie 头,gin 的 SetCookie 不暴露 SameSite 属性。
func (h *Handler) setRefreshCookie(c *gin.Context, plainToken string) {
	attributes := []string{
		RefreshTokenCookieName + "=" + plainToken,
		"Path=" + refreshCookiePath,
		"Max-Age=" + strconv.Itoa(h.refreshMaxAge),
		"HttpOnly",
		"SameSite=Lax",
	}
	if h.secureCookie {
		attributes = append(attributes, "Secure")
	}
	c.Header("Set-Cookie", strings.Join(attributes, "; "))
}

// clearRefreshCookie 清除 refresh Cookie,刷新失败和登出都会调用。
func (h *Handler) clearRefreshCookie(c *gin.Context) {
	c.Header("Set-Cookie", fmt.Sprintf("%s=; Path=%s; Max-Age=0; HttpOnly; SameSite=Lax", RefreshTokenCookieName, refreshCookiePath))
}

// Me 读取当前登录用户资料，用户 ID 来自 JWT 中间件写入的上下文。
func (h *Handler) Me(c *gin.Context) {
	userID, ok := userIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid access token"})
		return
	}

	profile, err := h.service.GetProfile(c.Request.Context(), userID)
	if err != nil {
		writeProfileError(c, err)
		return
	}

	c.JSON(http.StatusOK, profileResponse(profile))
}

// Get 读取公开用户资料，用于访问他人主页。
func (h *Handler) Get(c *gin.Context) {
	userID, err := parsePositiveUserID(c.Param("userId"))
	if err != nil {
		writeProfileError(c, err)
		return
	}

	profile, err := h.service.GetPublicProfile(c.Request.Context(), userID)
	if err != nil {
		writeProfileError(c, err)
		return
	}

	c.JSON(http.StatusOK, publicProfileResponse(profile))
}

// UpdateMe 更新当前登录用户资料，请求体支持部分字段更新。
func (h *Handler) UpdateMe(c *gin.Context) {
	userID, ok := userIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid access token"})
		return
	}

	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	profile, err := h.service.UpdateProfile(c.Request.Context(), userID, req.Nickname, req.AvatarURL, req.Bio)
	if err != nil {
		writeProfileError(c, err)
		return
	}

	c.JSON(http.StatusOK, profileResponse(profile))
}

// publicProfileResponse 将应用层 Profile 转成公开 JSON 结构。
func publicProfileResponse(profile *applicationaccount.Profile) publicUserProfileResponse {
	return publicUserProfileResponse{
		ID:             profile.ID,
		Nickname:       profile.Nickname,
		AvatarURL:      profile.AvatarURL,
		Bio:            profile.Bio,
		FollowingCount: profile.FollowingCount,
		FollowerCount:  profile.FollowerCount,
		WorkCount:      profile.WorkCount,
	}
}

// profileResponse 将应用层 Profile 转成对外 JSON 结构。
func profileResponse(profile *applicationaccount.Profile) userProfileResponse {
	return userProfileResponse{
		ID:             profile.ID,
		Account:        profile.Account,
		Nickname:       profile.Nickname,
		AvatarURL:      profile.AvatarURL,
		Bio:            profile.Bio,
		Status:         profile.Status,
		Role:           profile.Role,
		FollowingCount: profile.FollowingCount,
		FollowerCount:  profile.FollowerCount,
		WorkCount:      profile.WorkCount,
	}
}

// userIDFromContext 从 JWT 中间件写入的上下文中读取登录用户 ID。
func userIDFromContext(c *gin.Context) (int64, bool) {
	value, exists := c.Get(interfaceshttpmiddleware.ContextUserIDKey)
	if !exists {
		return 0, false
	}
	userID, ok := value.(int64)
	return userID, ok && userID > 0
}

func parsePositiveUserID(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0, domainaccount.ErrInvalidUserID
	}
	return value, nil
}

// writeProfileError 统一账号资料相关接口的错误响应。
func writeProfileError(c *gin.Context, err error) {
	if isBadRequestError(err) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, domainaccount.ErrUserNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal server error"})
}

// isBadRequestError 判断哪些领域错误属于客户端请求参数问题。
func isBadRequestError(err error) bool {
	return errors.Is(err, domainaccount.ErrEmptyAccount) ||
		errors.Is(err, domainaccount.ErrEmptyPassword) ||
		errors.Is(err, domainaccount.ErrEmptyNickname) ||
		errors.Is(err, domainaccount.ErrInvalidUserID) ||
		errors.Is(err, domainaccount.ErrEmptyProfileUpdate)
}
