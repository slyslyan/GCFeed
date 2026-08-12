package infrarecommendation

import (
	domainrecommendation "GCFeed/internal/domain/recommendation"
	domainvideo "GCFeed/internal/domain/video"
	infravector "GCFeed/internal/infra/vector"
	"context"
	"time"

	"gorm.io/gorm"
)

const followingRecallWindow = 72 * time.Hour
const vectorRecallAgeWindow = 30 * 24 * time.Hour
const vectorANNMultiplier = 3
const vectorANNMaxLimit = 1500

// HotRecaller 热度路：按 hot_score 倒序召回，曝光过滤由合并器统一处理。
type HotRecaller struct {
	db *gorm.DB
}

func NewHotRecaller(db *gorm.DB) *HotRecaller {
	return &HotRecaller{db: db}
}

func (r *HotRecaller) Recall(ctx context.Context, in domainrecommendation.RecallInput) ([]*domainrecommendation.Candidate, error) {
	if r == nil || r.db == nil || in.Limit <= 0 {
		return []*domainrecommendation.Candidate{}, nil
	}

	var models []candidateModel
	err := r.db.WithContext(ctx).
		Table("video AS v").
		Select("v.id AS video_id, v.author_id, ("+hotScoreExpression+") AS hot_score, v.published_at").
		Joins("LEFT JOIN video_stat AS vs ON vs.video_id = v.id").
		Where("v.status = ? AND v.published_at IS NOT NULL", domainvideo.StatusPublished).
		Order("hot_score DESC").
		Order("v.published_at DESC").
		Order("v.id DESC").
		Limit(in.Limit).
		Scan(&models).
		Error
	if err != nil {
		return nil, err
	}

	candidates := make([]*domainrecommendation.Candidate, 0, len(models))
	for _, model := range models {
		candidates = append(candidates, domainrecommendation.RestoreCandidate(
			model.VideoID,
			model.AuthorID,
			0,
			0,
			model.HotScore,
			0,
			"",
			model.PublishedAt,
			domainrecommendation.SourceHot,
		))
	}
	return candidates, nil
}

// FollowingRecaller 关注路：关注作者 72h 内新视频，发布时间倒序（最新优先，与 fresh boost 呼应）。
type FollowingRecaller struct {
	db *gorm.DB
}

func NewFollowingRecaller(db *gorm.DB) *FollowingRecaller {
	return &FollowingRecaller{db: db}
}

func (r *FollowingRecaller) Recall(ctx context.Context, in domainrecommendation.RecallInput) ([]*domainrecommendation.Candidate, error) {
	if r == nil || r.db == nil || in.Limit <= 0 {
		return []*domainrecommendation.Candidate{}, nil
	}
	// 双保险：SQL LIMIT + 条数硬上限，大 V 刷屏也撑不爆候选池。
	limit := in.Limit
	if limit > domainrecommendation.FollowingRecallLimitCap {
		limit = domainrecommendation.FollowingRecallLimitCap
	}

	var models []candidateModel
	err := r.db.WithContext(ctx).
		Table("video AS v").
		Select("v.id AS video_id, v.author_id, ("+hotScoreExpression+") AS hot_score, v.published_at").
		Joins("JOIN user_follow AS f ON f.user_id = ? AND f.target_user_id = v.author_id AND f.status = 1", in.UserID).
		Joins("LEFT JOIN video_stat AS vs ON vs.video_id = v.id").
		Where("v.status = ? AND v.published_at >= ? AND v.published_at IS NOT NULL", domainvideo.StatusPublished, in.Now.Add(-followingRecallWindow)).
		Order("v.published_at DESC").
		Order("v.id DESC").
		Limit(limit).
		Scan(&models).
		Error
	if err != nil {
		return nil, err
	}

	candidates := make([]*domainrecommendation.Candidate, 0, len(models))
	for _, model := range models {
		candidates = append(candidates, domainrecommendation.RestoreCandidate(
			model.VideoID,
			model.AuthorID,
			0,
			0,
			model.HotScore,
			0,
			"",
			model.PublishedAt,
			domainrecommendation.SourceFollowing,
		))
	}
	return candidates, nil
}

// VectorRecaller 向量路：用户兴趣向量在 Milvus 中 ANN 检索 topK。
// 降级：无用户向量或查询失败 → 由调用方丢弃该路结果，退化为双路，不阻塞主链路。
type VectorRecaller struct {
	db    *gorm.DB
	store infravector.VectorStore
}

func NewVectorRecaller(db *gorm.DB, store infravector.VectorStore) *VectorRecaller {
	return &VectorRecaller{db: db, store: store}
}

func (r *VectorRecaller) Recall(ctx context.Context, in domainrecommendation.RecallInput) ([]*domainrecommendation.Candidate, error) {
	if r == nil || r.db == nil || r.store == nil || in.Limit <= 0 || !in.HasUserVector || len(in.QueryVector) == 0 {
		return []*domainrecommendation.Candidate{}, nil
	}

	annLimit := in.Limit * vectorANNMultiplier
	if annLimit > vectorANNMaxLimit {
		annLimit = vectorANNMaxLimit
	}
	hits, err := r.store.Search(ctx, in.QueryVector, annLimit)
	if err != nil {
		return nil, err
	}
	// Milvus ANN 查询不带业务过滤，状态/时间过滤在应用层批量完成。
	videoIDs := make([]int64, 0, len(hits))
	for _, hit := range hits {
		videoIDs = append(videoIDs, hit.VideoID)
	}
	enriched, err := r.enrichVideos(ctx, videoIDs, in.Now)
	if err != nil {
		return nil, err
	}

	candidates := make([]*domainrecommendation.Candidate, 0, len(hits))
	for _, hit := range hits {
		candidate, ok := enriched[hit.VideoID]
		if !ok {
			continue
		}
		candidate.Similarity = milvusScoreToSimilarity(hit.Distance)
		candidate.Source = domainrecommendation.SourceVector
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// enrichVideos 批量富化候选（作者/热度/发布时间），过滤非发布状态与 30 天以上老旧视频。
func (r *VectorRecaller) enrichVideos(ctx context.Context, videoIDs []int64, now time.Time) (map[int64]*domainrecommendation.Candidate, error) {
	enriched := map[int64]*domainrecommendation.Candidate{}
	if len(videoIDs) == 0 {
		return enriched, nil
	}

	var models []candidateModel
	err := r.db.WithContext(ctx).
		Table("video AS v").
		Select("v.id AS video_id, v.author_id, ("+hotScoreExpression+") AS hot_score, v.published_at").
		Joins("LEFT JOIN video_stat AS vs ON vs.video_id = v.id").
		Where("v.id IN ? AND v.status = ? AND v.published_at >= ? AND v.published_at IS NOT NULL",
			videoIDs, domainvideo.StatusPublished, now.Add(-vectorRecallAgeWindow)).
		Scan(&models).
		Error
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		enriched[model.VideoID] = domainrecommendation.RestoreCandidate(
			model.VideoID,
			model.AuthorID,
			0,
			0,
			model.HotScore,
			0,
			"",
			model.PublishedAt,
		)
	}
	return enriched, nil
}

// milvusScoreToSimilarity 实测 Milvus v2.4.13 对 COSINE 度量直接返回余弦相似度
// （相同向量得 1，正交得 0），负相似度截断为 0。语义由 gated 测试 TestMilvusANN 钉死。
func milvusScoreToSimilarity(score float64) float64 {
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}
