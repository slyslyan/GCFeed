package infrasession

import (
	domainsession "GCFeed/internal/domain/session"
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

type Repository struct {
	db *gorm.DB
}

// New 创建 refresh token 会话仓储实现,db 由路由装配阶段注入。
func New(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Save(ctx context.Context, session *domainsession.RefreshSession) error {
	model := toModel(session)
	if err := r.db.WithContext(ctx).Create(&model).Error; err != nil {
		return err
	}
	session.ID = model.ID
	return nil
}

func (r *Repository) FindByHash(ctx context.Context, tokenHash string) (*domainsession.RefreshSession, error) {
	var model RefreshTokenModel
	err := r.db.WithContext(ctx).Where("token_hash = ?", tokenHash).Take(&model).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domainsession.ErrRefreshTokenNotFound
		}
		return nil, err
	}
	return toDomain(model), nil
}

// RevokeByHashIfActive 条件撤销:只对仍活跃的行写入撤销时间,RowsAffected 表示 CAS 是否成功。
func (r *Repository) RevokeByHashIfActive(ctx context.Context, tokenHash string, now time.Time) (bool, error) {
	result := r.db.WithContext(ctx).
		Model(&RefreshTokenModel{}).
		Where("token_hash = ? AND revoked_at IS NULL", tokenHash).
		Update("revoked_at", now)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// RevokeByFamily 整族撤销,重用检测触发时把家族内所有会话(含刚轮换出的新行)一次作废。
func (r *Repository) RevokeByFamily(ctx context.Context, familyID string, now time.Time) (int64, error) {
	result := r.db.WithContext(ctx).
		Model(&RefreshTokenModel{}).
		Where("family_id = ? AND revoked_at IS NULL", familyID).
		Update("revoked_at", now)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// Rotate 事务内完成"撤销旧行 + 插入新行",保证轮换原子性;
// 旧行已非活跃(并发/重复刷新)时返回 ErrRefreshTokenReuseDetected。
func (r *Repository) Rotate(ctx context.Context, oldHash string, newSession *domainsession.RefreshSession) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&RefreshTokenModel{}).
			Where("token_hash = ? AND revoked_at IS NULL", oldHash).
			Update("revoked_at", time.Now())
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return domainsession.ErrRefreshTokenReuseDetected
		}
		return tx.Create(toModel(newSession)).Error
	})
}

func toModel(session *domainsession.RefreshSession) *RefreshTokenModel {
	return &RefreshTokenModel{
		ID:        session.ID,
		UserID:    session.UserID,
		FamilyID:  session.FamilyID,
		TokenHash: session.TokenHash,
		ExpiresAt: session.ExpiresAt,
		RevokedAt: session.RevokedAt,
	}
}

func toDomain(model RefreshTokenModel) *domainsession.RefreshSession {
	return &domainsession.RefreshSession{
		ID:        model.ID,
		UserID:    model.UserID,
		FamilyID:  model.FamilyID,
		TokenHash: model.TokenHash,
		ExpiresAt: model.ExpiresAt,
		RevokedAt: model.RevokedAt,
		CreatedAt: model.CreatedAt,
	}
}
