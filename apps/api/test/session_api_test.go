package test

import (
	domainsession "GCFeed/internal/domain/session"
	infrarefreshtoken "GCFeed/internal/infra/refreshtoken"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// memorySessionRepo 是 refresh token 会话测试用的内存仓储,模拟 CAS 与撤销语义。
type memorySessionRepo struct {
	mu     sync.Mutex
	byHash map[string]*domainsession.RefreshSession
}

func newMemorySessionRepo() *memorySessionRepo {
	return &memorySessionRepo{byHash: map[string]*domainsession.RefreshSession{}}
}

func (r *memorySessionRepo) Save(ctx context.Context, session *domainsession.RefreshSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	clone := *session
	clone.ID = int64(len(r.byHash)) + 1
	r.byHash[session.TokenHash] = &clone
	return nil
}

func (r *memorySessionRepo) FindByHash(ctx context.Context, tokenHash string) (*domainsession.RefreshSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, exists := r.byHash[tokenHash]
	if !exists {
		return nil, domainsession.ErrRefreshTokenNotFound
	}
	clone := *session
	return &clone, nil
}

func (r *memorySessionRepo) RevokeByHashIfActive(ctx context.Context, tokenHash string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, exists := r.byHash[tokenHash]
	if !exists || session.RevokedAt != nil {
		return false, nil
	}
	session.RevokedAt = &now
	return true, nil
}

func (r *memorySessionRepo) RevokeByFamily(ctx context.Context, familyID string, now time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var affected int64
	for _, session := range r.byHash {
		if session.FamilyID == familyID && session.RevokedAt == nil {
			session.RevokedAt = &now
			affected++
		}
	}
	return affected, nil
}

func (r *memorySessionRepo) Rotate(ctx context.Context, oldHash string, newSession *domainsession.RefreshSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, exists := r.byHash[oldHash]
	if !exists {
		return domainsession.ErrRefreshTokenNotFound
	}
	if session.RevokedAt != nil {
		return domainsession.ErrRefreshTokenReuseDetected
	}
	now := time.Now()
	session.RevokedAt = &now
	r.byHash[newSession.TokenHash] = newSession
	return nil
}

// expireForTest 把会话过期时间拨到过去,模拟过期 refresh token。
func (r *memorySessionRepo) expireForTest(tokenHash string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if session, exists := r.byHash[tokenHash]; exists {
		session.ExpiresAt = time.Now().Add(-time.Hour)
	}
}

// getForTest 返回指定哈希对应的会话副本,测试直接操作会话状态用。
func (r *memorySessionRepo) getForTest(tokenHash string) *domainsession.RefreshSession {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, exists := r.byHash[tokenHash]
	if !exists {
		return nil
	}
	clone := *session
	return &clone
}

// loginAndGetCookie 注册并登录,返回 refresh token 明文(即响应 Cookie 的 value)。
func loginAndGetCookie(t *testing.T, router *gin.Engine) string {
	t.Helper()

	registerResponse := performJSONRequest(
		router,
		http.MethodPost,
		"/api/users",
		`{"account":"sessionuser","password":"12345678","nickname":"tester"}`,
		"",
	)
	requireStatus(t, registerResponse, http.StatusCreated)

	loginResponse := performJSONRequest(
		router,
		http.MethodPost,
		"/api/sessions",
		`{"account":"sessionuser","password":"12345678"}`,
		"",
	)
	requireStatus(t, loginResponse, http.StatusOK)

	cookie := cookieValue(t, loginResponse, "gcfeed_refresh_token")
	if cookie == "" {
		t.Fatalf("expected refresh cookie in login response, headers=%v", loginResponse.Header().Values("Set-Cookie"))
	}
	return cookie
}

// cookieValue 从响应 Set-Cookie 头中提取指定 cookie 的 value。
func cookieValue(t *testing.T, resp *httptest.ResponseRecorder, name string) string {
	t.Helper()
	for _, line := range resp.Header().Values("Set-Cookie") {
		parts := strings.SplitN(line, ";", 2)
		kv := strings.SplitN(strings.TrimSpace(parts[0]), "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return kv[1]
		}
	}
	return ""
}

// performCookieRequest 带 refresh cookie 发起请求,用于刷新/登出接口。
func performCookieRequest(router *gin.Engine, method, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBuffer(nil))
	if cookie != "" {
		req.Header.Set("Cookie", "gcfeed_refresh_token="+cookie)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// TestSessionLoginSetsRefreshCookie 验证登录响应通过 httpOnly Cookie 下发 refresh token,属性完整。
func TestSessionLoginSetsRefreshCookie(t *testing.T) {
	router := newAccountRouter(t)

	registerResponse := performJSONRequest(
		router,
		http.MethodPost,
		"/api/users",
		`{"account":"cookieuser","password":"12345678","nickname":"tester"}`,
		"",
	)
	requireStatus(t, registerResponse, http.StatusCreated)

	loginResponse := performJSONRequest(
		router,
		http.MethodPost,
		"/api/sessions",
		`{"account":"cookieuser","password":"12345678"}`,
		"",
	)
	requireStatus(t, loginResponse, http.StatusOK)

	var setCookie string
	for _, line := range loginResponse.Header().Values("Set-Cookie") {
		if strings.HasPrefix(line, "gcfeed_refresh_token=") {
			setCookie = line
			break
		}
	}
	if setCookie == "" {
		t.Fatalf("expected refresh cookie in login response, headers=%v", loginResponse.Header().Values("Set-Cookie"))
	}
	for _, attr := range []string{"Path=/api/sessions", "HttpOnly", "SameSite=Lax", "Max-Age=604800"} {
		if !strings.Contains(setCookie, attr) {
			t.Fatalf("cookie %q missing attribute %q", setCookie, attr)
		}
	}
}

// TestSessionRefreshSuccess 验证刷新成功:新 access token 与旧不同,refresh token 轮换。
func TestSessionRefreshSuccess(t *testing.T) {
	router := newAccountRouter(t)
	cookie := loginAndGetCookie(t, router)

	firstRefresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, firstRefresh, http.StatusOK)

	var first accountTokenResponse
	decodeJSON(t, firstRefresh, &first)
	if first.AccessToken == "" {
		t.Fatalf("expected new access token")
	}
	newCookie := cookieValue(t, firstRefresh, "gcfeed_refresh_token")
	if newCookie == "" || newCookie == cookie {
		t.Fatalf("expected rotated refresh cookie, got %q", newCookie)
	}
}

// TestSessionRefreshOldTokenRejected 验证轮换后旧 refresh token 立即失效。
func TestSessionRefreshOldTokenRejected(t *testing.T) {
	router := newAccountRouter(t)
	cookie := loginAndGetCookie(t, router)

	firstRefresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, firstRefresh, http.StatusOK)

	oldReuse := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, oldReuse, http.StatusUnauthorized)
	// 旧 token 被拒同时下发清 Cookie。
	if got := cookieValue(t, oldReuse, "gcfeed_refresh_token"); got != "" {
		t.Fatalf("expected cleared cookie on rejected refresh, got %q", got)
	}
}

// TestSessionRefreshReuseDetection 验证重用检测:旧 token 被再次使用会撤掉整个家族。
func TestSessionRefreshReuseDetection(t *testing.T) {
	router := newAccountRouter(t)
	cookie := loginAndGetCookie(t, router)

	firstRefresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, firstRefresh, http.StatusOK)
	newCookie := cookieValue(t, firstRefresh, "gcfeed_refresh_token")

	// 用已轮换的旧 token 触发重用 → 401,家族被撤。
	reuse := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, reuse, http.StatusUnauthorized)

	// 家族已撤,连刚轮换出的新 token 也失效。
	newTokenReuse := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", newCookie)
	requireStatus(t, newTokenReuse, http.StatusUnauthorized)
}

// TestSessionLogoutRevokesRefresh 验证登出后 refresh token 失效。
func TestSessionLogoutRevokesRefresh(t *testing.T) {
	router := newAccountRouter(t)
	cookie := loginAndGetCookie(t, router)

	logout := performCookieRequest(router, http.MethodDelete, "/api/sessions/current", cookie)
	requireStatus(t, logout, http.StatusNoContent)

	refresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, refresh, http.StatusUnauthorized)
}

// TestSessionRefreshWithoutCookie 验证无 Cookie 刷新返回 401。
func TestSessionRefreshWithoutCookie(t *testing.T) {
	router := newAccountRouter(t)
	refresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", "")
	requireStatus(t, refresh, http.StatusUnauthorized)
}

// TestSessionRefreshExpired 验证过期 refresh token 被拒,且不撤族(同族其他活跃 token 仍可用)。
func TestSessionRefreshExpired(t *testing.T) {
	router, _, sessionRepo := newAccountRouterWithRepo(t)
	cookie := loginAndGetCookie(t, router)

	firstRefresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, firstRefresh, http.StatusOK)
	newCookie := cookieValue(t, firstRefresh, "gcfeed_refresh_token")

	gen, err := infrarefreshtoken.NewGenerator("168h")
	if err != nil {
		t.Fatalf("new generator: %v", err)
	}
	newHash := gen.Hash(newCookie)
	sessionRepo.expireForTest(newHash)
	expired := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", newCookie)
	requireStatus(t, expired, http.StatusUnauthorized)

	// 过期只是生命周期结束,不撤族:同族补一个活跃会话,刷新仍应成功。
	expiredSession := sessionRepo.getForTest(newHash)
	if expiredSession == nil {
		t.Fatalf("expected session row for hash %q", newHash)
	}
	survivor := &domainsession.RefreshSession{
		UserID:    expiredSession.UserID,
		FamilyID:  expiredSession.FamilyID,
		TokenHash: gen.Hash("family-survivor-plain"),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := sessionRepo.Save(context.Background(), survivor); err != nil {
		t.Fatalf("save survivor session: %v", err)
	}
	survivorRefresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", "family-survivor-plain")
	requireStatus(t, survivorRefresh, http.StatusOK)
}

// TestSessionLogoutIdempotent 验证登出幂等:无 Cookie/重复登出都返回 204。
func TestSessionLogoutIdempotent(t *testing.T) {
	router := newAccountRouter(t)

	first := performCookieRequest(router, http.MethodDelete, "/api/sessions/current", "")
	requireStatus(t, first, http.StatusNoContent)
	second := performCookieRequest(router, http.MethodDelete, "/api/sessions/current", "")
	requireStatus(t, second, http.StatusNoContent)
}

// TestSessionLogoutClearsCookie 验证登出响应清除 refresh Cookie。
func TestSessionLogoutClearsCookie(t *testing.T) {
	router := newAccountRouter(t)
	cookie := loginAndGetCookie(t, router)

	logout := performCookieRequest(router, http.MethodDelete, "/api/sessions/current", cookie)
	requireStatus(t, logout, http.StatusNoContent)
	if got := cookieValue(t, logout, "gcfeed_refresh_token"); got != "" {
		t.Fatalf("expected cleared cookie on logout, got %q", got)
	}
}

// TestSessionConcurrentRefresh 验证并发刷新同一 token 时恰好一个成功,随后全族失效。
func TestSessionConcurrentRefresh(t *testing.T) {
	router := newAccountRouter(t)
	cookie := loginAndGetCookie(t, router)

	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			refresh := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
			results <- refresh.Code
		}()
	}

	statuses := map[int]int{}
	for i := 0; i < 2; i++ {
		statuses[<-results]++
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusUnauthorized] != 1 {
		t.Fatalf("expected exactly one success and one 401, got %v", statuses)
	}

	// 撤族后,任何刷新都不再成功。
	after := performCookieRequest(router, http.MethodPost, "/api/sessions/refresh", cookie)
	requireStatus(t, after, http.StatusUnauthorized)
}
