package infrasession

import (
	"time"
)

// RefreshTokenModel 映射 refresh_token 表,只存 token 的 SHA-256 哈希。
type RefreshTokenModel struct {
	ID        int64      `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    int64      `gorm:"column:user_id;not null;index"`
	FamilyID  string     `gorm:"column:family_id;size:32;not null;index"`
	TokenHash string     `gorm:"column:token_hash;size:64;not null;uniqueIndex"`
	ExpiresAt time.Time  `gorm:"column:expires_at;not null;index"`
	RevokedAt *time.Time `gorm:"column:revoked_at;default:null"`
	CreatedAt time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time  `gorm:"column:updated_at;autoUpdateTime"`
}

func (RefreshTokenModel) TableName() string {
	return "refresh_token"
}
