package refreshtoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	infrajwt "GCFeed/internal/infra/jwt"
)

const defaultRefreshTTL = 7 * 24 * time.Hour

// Generator 生成不透明 refresh token:32 字节随机熵,库中只存 SHA-256 哈希。
type Generator struct {
	ttl time.Duration
}

func NewGenerator(refreshTTL string) (*Generator, error) {
	ttl, err := infrajwt.ParseTTL(refreshTTL, defaultRefreshTTL)
	if err != nil {
		return nil, err
	}
	return &Generator{ttl: ttl}, nil
}

// Generate 生成新的明文 refresh token,调用方负责把它交给客户端并只保存哈希。
func (g *Generator) Generate() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Hash 计算 token 的 SHA-256 十六进制摘要,是库中存储的唯一形态。
func (g *Generator) Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// TTL 返回 refresh token 的有效期,登录和刷新写库、下发 Cookie 都要对齐它。
func (g *Generator) TTL() time.Duration {
	return g.ttl
}
