package domainembedding

import "context"

// Repository 定义 embedding 模块需要的持久化能力。
type Repository interface {
	SaveVideoEmbedding(ctx context.Context, embedding *VideoEmbedding) error
	FindVideoEmbedding(ctx context.Context, videoID int64, model string) (*VideoEmbedding, error)
	// ListAllVideoEmbeddings 按 video_id 递增分页遍历全部向量，供存量回填向量库。
	ListAllVideoEmbeddings(ctx context.Context, afterID int64, limit int) ([]*VideoEmbedding, error)
}
