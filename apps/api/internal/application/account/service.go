package applicationaccount

import (
	domainaccount "GCFeed/internal/domain/account"
	domainsession "GCFeed/internal/domain/session"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var ErrLoadAccountFailed = errors.New("failed to load account")
var ErrSaveAccountFailed = errors.New("failed to save account")
var ErrUpdateAccountFailed = errors.New("failed to update account")
var ErrSignAccessTokenFailed = errors.New("failed to sign access token")
var ErrGenerateRefreshTokenFailed = errors.New("failed to generate refresh token")
var ErrSaveRefreshSessionFailed = errors.New("failed to save refresh session")
var ErrLoadRefreshSessionFailed = errors.New("failed to load refresh session")
var ErrRotateRefreshSessionFailed = errors.New("failed to rotate refresh session")
var ErrRevokeRefreshSessionFailed = errors.New("failed to revoke refresh session")

// TokenSigner 是应用层依赖的最小 JWT 能力，账号服务只关心“签发 token”和“过期时间”。
type TokenSigner interface {
	SignAccessToken(userID int64, role string) (string, error)
	AccessTTL() time.Duration
}

// RefreshTokenGenerator 生成不透明 refresh token 并计算其哈希，哈希是库中存储的唯一形态。
type RefreshTokenGenerator interface {
	Generate() (string, error)
	Hash(plain string) string
	TTL() time.Duration
}

// Service 编排账号用例：注册、登录、刷新会话、登出、读取资料、更新资料。
type Service struct {
	repo     domainaccount.Repository
	signer   TokenSigner
	refresh  RefreshTokenGenerator
	sessions domainsession.Repository
}

// LoginResult 是登录成功后返回给 HTTP 层的 token 数据。
type LoginResult struct {
	AccessToken      string
	TokenType        string
	ExpiresInSeconds int64
	RefreshToken     string
}

// RefreshResult 是刷新成功后返回给 HTTP 层的 token 数据，与登录结果同构。
type RefreshResult struct {
	AccessToken      string
	TokenType        string
	ExpiresInSeconds int64
	RefreshToken     string
}

// Profile 是应用层对外暴露的用户资料视图，屏蔽密码等敏感字段。
type Profile struct {
	ID             int64
	Account        string
	Nickname       string
	AvatarURL      string
	Bio            string
	Status         int
	Role           string
	FollowingCount int
	FollowerCount  int
	WorkCount      int
}

func New(repo domainaccount.Repository, signer TokenSigner, refresh RefreshTokenGenerator, sessions domainsession.Repository) *Service {
	return &Service{
		repo:     repo,
		signer:   signer,
		refresh:  refresh,
		sessions: sessions,
	}
}

// Register 创建新用户：领域层负责校验和加密密码，仓储层负责持久化。
func (s *Service) Register(ctx context.Context, account, password, nickname string) (*Profile, error) {
	user, err := domainaccount.New(account, password, nickname)
	if err != nil {
		return nil, err
	}

	err = s.repo.Save(ctx, user)
	if err != nil {
		if errors.Is(err, domainaccount.ErrAccountAlreadyExists) {
			return nil, domainaccount.ErrAccountAlreadyExists
		}
		return nil, ErrSaveAccountFailed
	}
	return profileFromUser(user), nil
}

// Login 完成账号密码登录，认证通过后签发访问 token。
func (s *Service) Login(ctx context.Context, account, password string) (*LoginResult, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, domainaccount.ErrEmptyAccount
	}

	user, err := s.repo.FindByAccount(ctx, account)
	if err != nil {
		if errors.Is(err, domainaccount.ErrUserNotFound) {
			return nil, domainaccount.ErrInvalidCredentials
		}
		return nil, ErrLoadAccountFailed
	}
	if err := user.Authenticate(password); err != nil {
		return nil, err
	}

	// token 内写入用户 ID 和角色，后续鉴权中间件会解析并放入请求上下文。
	accessToken, err := s.signer.SignAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, ErrSignAccessTokenFailed
	}

	refreshToken, err := s.issueRefreshSession(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	return &LoginResult{
		AccessToken:      accessToken,
		TokenType:        "Bearer",
		ExpiresInSeconds: int64(s.signer.AccessTTL().Seconds()),
		RefreshToken:     refreshToken,
	}, nil
}

// issueRefreshSession 生成新家族会话并落库:登录建新 family。
func (s *Service) issueRefreshSession(ctx context.Context, userID int64) (string, error) {
	familyID, err := newFamilyID()
	if err != nil {
		return "", ErrGenerateRefreshTokenFailed
	}
	plain, session, err := s.buildRefreshSession(userID, familyID)
	if err != nil {
		return "", err
	}
	if err := s.sessions.Save(ctx, session); err != nil {
		return "", ErrSaveRefreshSessionFailed
	}
	return plain, nil
}

// buildRefreshSession 生成明文 token 和对应的领域会话,不落库——
// 登录由 Save 落库,刷新由 Rotate 在事务内落库。
func (s *Service) buildRefreshSession(userID int64, familyID string) (string, *domainsession.RefreshSession, error) {
	plain, err := s.refresh.Generate()
	if err != nil {
		return "", nil, ErrGenerateRefreshTokenFailed
	}
	session, err := domainsession.NewRefreshSession(userID, familyID, s.refresh.Hash(plain), s.refresh.TTL())
	if err != nil {
		return "", nil, err
	}
	return plain, session, nil
}

// Refresh 用 refresh token 换取新的 token 对:先过服务端状态校验,
// 再 CAS 轮换——旧 token 立即失效,若已失效则判定重用并整族撤销。
func (s *Service) Refresh(ctx context.Context, plainToken string) (*RefreshResult, error) {
	plainToken = strings.TrimSpace(plainToken)
	if plainToken == "" {
		return nil, domainsession.ErrRefreshTokenNotFound
	}

	session, err := s.sessions.FindByHash(ctx, s.refresh.Hash(plainToken))
	if err != nil {
		if errors.Is(err, domainsession.ErrRefreshTokenNotFound) {
			return nil, err
		}
		return nil, ErrLoadRefreshSessionFailed
	}

	now := time.Now()
	if session.RevokedAt != nil {
		// 已被轮换/撤销的 token 再次出现 = 重用嫌疑,整族作废(含并发赢家刚签发的新行)。
		if _, err := s.sessions.RevokeByFamily(ctx, session.FamilyID, now); err != nil {
			return nil, ErrRevokeRefreshSessionFailed
		}
		return nil, domainsession.ErrRefreshTokenReuseDetected
	}
	if session.IsExpired(now) {
		// 过期是正常生命周期结束,不撤族。
		return nil, domainsession.ErrRefreshTokenExpired
	}

	// 同一家族内签发新 token 并撤销旧 token,CAS 失败说明已被并发刷新,同样视为重用。
	newPlain, newSession, err := s.buildRefreshSession(session.UserID, session.FamilyID)
	if err != nil {
		return nil, err
	}
	if err := s.sessions.Rotate(ctx, session.TokenHash, newSession); err != nil {
		if errors.Is(err, domainsession.ErrRefreshTokenReuseDetected) {
			if _, revokeErr := s.sessions.RevokeByFamily(ctx, session.FamilyID, now); revokeErr != nil {
				return nil, ErrRevokeRefreshSessionFailed
			}
			return nil, err
		}
		return nil, ErrRotateRefreshSessionFailed
	}

	// 实时取用户角色签发新 access token,角色变更立即生效。
	user, err := s.repo.FindByID(ctx, session.UserID)
	if err != nil {
		return nil, ErrLoadAccountFailed
	}
	accessToken, err := s.signer.SignAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, ErrSignAccessTokenFailed
	}

	return &RefreshResult{
		AccessToken:      accessToken,
		TokenType:        "Bearer",
		ExpiresInSeconds: int64(s.signer.AccessTTL().Seconds()),
		RefreshToken:     newPlain,
	}, nil
}

// Logout 只撤销当前会话的 refresh token,不撤族;找不到也返回成功,保证幂等。
func (s *Service) Logout(ctx context.Context, plainToken string) error {
	plainToken = strings.TrimSpace(plainToken)
	if plainToken == "" {
		return nil
	}
	if _, err := s.sessions.RevokeByHashIfActive(ctx, s.refresh.Hash(plainToken), time.Now()); err != nil {
		return ErrRevokeRefreshSessionFailed
	}
	return nil
}

// newFamilyID 生成随机家族 ID,登录时一次性创建。
func newFamilyID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// GetProfile 根据登录态中的用户 ID 读取当前用户资料。
func (s *Service) GetProfile(ctx context.Context, userID int64) (*Profile, error) {
	if userID <= 0 {
		return nil, domainaccount.ErrInvalidUserID
	}

	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, domainaccount.ErrUserNotFound) {
			return nil, domainaccount.ErrUserNotFound
		}
		return nil, ErrLoadAccountFailed
	}

	return profileFromUser(user), nil
}

// GetPublicProfile 根据用户 ID 读取公开资料，用于访问他人主页。
func (s *Service) GetPublicProfile(ctx context.Context, userID int64) (*Profile, error) {
	if userID <= 0 {
		return nil, domainaccount.ErrInvalidUserID
	}

	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, domainaccount.ErrUserNotFound) {
			return nil, domainaccount.ErrUserNotFound
		}
		return nil, ErrLoadAccountFailed
	}

	return profileFromUser(user), nil
}

// UpdateProfile 支持部分更新，nil 表示该字段没有出现在请求体中。
func (s *Service) UpdateProfile(ctx context.Context, userID int64, nickname, avatarURL, bio *string) (*Profile, error) {
	if userID <= 0 {
		return nil, domainaccount.ErrInvalidUserID
	}

	user, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, domainaccount.ErrUserNotFound) {
			return nil, domainaccount.ErrUserNotFound
		}
		return nil, ErrLoadAccountFailed
	}
	if err := user.UpdateProfile(nickname, avatarURL, bio); err != nil {
		return nil, err
	}
	if err := s.repo.UpdateProfile(ctx, user); err != nil {
		if errors.Is(err, domainaccount.ErrUserNotFound) {
			return nil, domainaccount.ErrUserNotFound
		}
		return nil, ErrUpdateAccountFailed
	}

	return profileFromUser(user), nil
}

// profileFromUser 把领域用户转换成安全的资料对象，避免向外暴露密码哈希。
func profileFromUser(user *domainaccount.User) *Profile {
	return &Profile{
		ID:             user.ID,
		Account:        user.Account,
		Nickname:       user.Nickname,
		AvatarURL:      user.AvatarURL,
		Bio:            user.Bio,
		Status:         user.Status,
		Role:           user.Role,
		FollowingCount: user.FollowingCount,
		FollowerCount:  user.FollowerCount,
		WorkCount:      user.WorkCount,
	}
}
