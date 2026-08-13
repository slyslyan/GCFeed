package domainsession

import (
	"context"
	"time"
)

// Repository 负责 refresh token 会话的持久化,行只软撤销不物理删除,
// 重用检测依赖"已撤销的行仍能被哈希命中"。
type Repository interface {
	Save(ctx context.Context, session *RefreshSession) error

	// FindByHash 按哈希精确查会话,未命中返回 ErrRefreshTokenNotFound。
	FindByHash(ctx context.Context, tokenHash string) (*RefreshSession, error)

	// RevokeByHashIfActive 条件撤销(CAS):仅当行仍活跃(revoked_at IS NULL)时写入撤销时间。
	RevokeByHashIfActive(ctx context.Context, tokenHash string, now time.Time) (bool, error)

	// RevokeByFamily 批量撤销整个家族,返回受影响行数,重用检测触发时调用。
	RevokeByFamily(ctx context.Context, familyID string, now time.Time) (int64, error)

	// Rotate 在事务内撤销旧行并插入新行;旧行已非活跃时返回 ErrRefreshTokenReuseDetected。
	Rotate(ctx context.Context, oldHash string, newSession *RefreshSession) error
}
