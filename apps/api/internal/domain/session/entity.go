package domainsession

import (
	"strings"
	"time"
)

// RefreshSession 是一次登录会话对应的服务端状态,只存 refresh token 的哈希。
type RefreshSession struct {
	ID        int64
	UserID    int64
	FamilyID  string
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// NewRefreshSession 校验会话字段并构造领域对象,ttl 决定过期时间。
func NewRefreshSession(userID int64, familyID, tokenHash string, ttl time.Duration) (*RefreshSession, error) {
	familyID = strings.TrimSpace(familyID)
	tokenHash = strings.TrimSpace(tokenHash)
	if userID <= 0 {
		return nil, ErrInvalidUserID
	}
	if familyID == "" {
		return nil, ErrInvalidFamilyID
	}
	if tokenHash == "" {
		return nil, ErrInvalidTokenHash
	}
	if ttl <= 0 {
		return nil, ErrInvalidRefreshTTL
	}

	return &RefreshSession{
		UserID:    userID,
		FamilyID:  familyID,
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// IsExpired 判断会话是否已超过有效期,过期是正常生命周期结束,不触发撤族。
func (s *RefreshSession) IsExpired(now time.Time) bool {
	return s.ExpiresAt.Before(now)
}

// Revoke 标记会话撤销,轮换和登出都会调用。
func (s *RefreshSession) Revoke(now time.Time) {
	s.RevokedAt = &now
}
