package applicationrecommendation

import (
	domainrecommendation "GCFeed/internal/domain/recommendation"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRepo struct {
	userVector []float64
	hasVector  bool
	exposed    map[int64]bool
}

func (f *fakeRepo) ListCandidatePool(ctx context.Context, userID int64, limit int) ([]*domainrecommendation.Candidate, error) {
	return []*domainrecommendation.Candidate{}, nil
}
func (f *fakeRepo) LoadUserInterestVector(ctx context.Context, userID int64) ([]float64, bool, error) {
	return f.userVector, f.hasVector, nil
}
func (f *fakeRepo) LoadVideoVectors(ctx context.Context, videoIDs []int64) (map[int64][]float64, error) {
	return map[int64][]float64{}, nil
}
func (f *fakeRepo) ListRecentExposures(ctx context.Context, userID int64, videoIDs []int64, since time.Time) ([]*domainrecommendation.Exposure, error) {
	var exposures []*domainrecommendation.Exposure
	for _, id := range videoIDs {
		if f.exposed[id] {
			exposures = append(exposures, domainrecommendation.RestoreExposure(1, userID, id, time.Now(), time.Now(), 1, "recommend"))
		}
	}
	return exposures, nil
}
func (f *fakeRepo) SaveExposures(ctx context.Context, writes []*domainrecommendation.ExposureWrite) ([]*domainrecommendation.Exposure, error) {
	return []*domainrecommendation.Exposure{}, nil
}

type fakeRecaller struct {
	source string
	cands  []*domainrecommendation.Candidate
	err    error
	delay  time.Duration
	bloc   chan struct{}
	once   sync.Once
}

func (f *fakeRecaller) Recall(ctx context.Context, in domainrecommendation.RecallInput) ([]*domainrecommendation.Candidate, error) {
	if f.bloc != nil {
		<-f.bloc
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.cands, nil
}

func candidate(id int64, hot int, similarity float64, source string) *domainrecommendation.Candidate {
	return domainrecommendation.RestoreCandidate(
		id, 1, 0, similarity, hot, 0, "", time.Now().Add(-time.Hour), source,
	)
}

func TestRecommend_RecallCandidates(t *testing.T) {
	now := time.Now()

	t.Run("三路正常合并：去重 + 向量相似度保留 + following 配额", func(t *testing.T) {
		repo := &fakeRepo{userVector: []float64{1, 0, 0}, hasVector: true}
		vector := &fakeRecaller{source: domainrecommendation.SourceVector, cands: []*domainrecommendation.Candidate{
			candidate(1, 10, 0.9, domainrecommendation.SourceVector),
			candidate(2, 20, 0.8, domainrecommendation.SourceVector),
		}}
		following := &fakeRecaller{source: domainrecommendation.SourceFollowing, cands: []*domainrecommendation.Candidate{
			candidate(2, 5, 0, domainrecommendation.SourceFollowing),
			candidate(3, 3, 0, domainrecommendation.SourceFollowing),
		}}
		hot := &fakeRecaller{source: domainrecommendation.SourceHot, cands: []*domainrecommendation.Candidate{
			candidate(1, 100, 0, domainrecommendation.SourceHot),
			candidate(4, 50, 0, domainrecommendation.SourceHot),
		}}
		service := New(repo, WithRecallerSet([]domainrecommendation.Recaller{vector, following, hot}), WithNow(func() time.Time { return now }))

		pool, hasUserVector, err := service.recallCandidates(context.Background(), 42, 50)
		if err != nil {
			t.Fatalf("recall failed: %v", err)
		}
		if !hasUserVector {
			t.Error("expected hasUserVector true")
		}
		byID := map[int64]*domainrecommendation.Candidate{}
		for _, c := range pool {
			byID[c.VideoID] = c
		}
		if len(byID) != 4 {
			t.Fatalf("expected 4 deduped candidates, got %d", len(byID))
		}
		// 视频 1 跨路重复：保留 vector 来源（优先级最高），HotScore 取最大值 100，Similarity 保留。
		c1 := byID[1]
		if c1.Source != domainrecommendation.SourceVector {
			t.Errorf("video 1: expected source vector, got %s", c1.Source)
		}
		if c1.HotScore != 100 {
			t.Errorf("video 1: expected hot score 100, got %d", c1.HotScore)
		}
		if c1.Similarity != 0.9 {
			t.Errorf("video 1: expected similarity 0.9, got %f", c1.Similarity)
		}
	})

	t.Run("向量路失败降级为双路，主链路不报错", func(t *testing.T) {
		repo := &fakeRepo{userVector: []float64{1, 0, 0}, hasVector: true}
		vector := &fakeRecaller{source: domainrecommendation.SourceVector, err: errors.New("ann failed")}
		following := &fakeRecaller{source: domainrecommendation.SourceFollowing, cands: []*domainrecommendation.Candidate{
			candidate(3, 3, 0, domainrecommendation.SourceFollowing),
		}}
		hot := &fakeRecaller{source: domainrecommendation.SourceHot, cands: []*domainrecommendation.Candidate{
			candidate(4, 50, 0, domainrecommendation.SourceHot),
		}}
		service := New(repo, WithRecallerSet([]domainrecommendation.Recaller{vector, following, hot}), WithNow(func() time.Time { return now }))

		pool, _, err := service.recallCandidates(context.Background(), 42, 50)
		if err != nil {
			t.Fatalf("recall should not fail when vector route fails: %v", err)
		}
		if len(pool) != 2 {
			t.Fatalf("expected 2 candidates from two routes, got %d", len(pool))
		}
	})

	t.Run("向量路超时丢弃该路，其他路正常", func(t *testing.T) {
		repo := &fakeRepo{userVector: []float64{1, 0, 0}, hasVector: true}
		blocked := make(chan struct{})
		defer close(blocked)
		vector := &fakeRecaller{source: domainrecommendation.SourceVector, bloc: blocked}
		hot := &fakeRecaller{source: domainrecommendation.SourceHot, cands: []*domainrecommendation.Candidate{
			candidate(4, 50, 0, domainrecommendation.SourceHot),
		}}
		service := New(repo, WithRecallerSet([]domainrecommendation.Recaller{vector, hot}), WithNow(func() time.Time { return now }))

		start := time.Now()
		pool, _, err := service.recallCandidates(context.Background(), 42, 50)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("recall should not fail on route timeout: %v", err)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("timeout not honored: took %s", elapsed)
		}
		if len(pool) != 1 || pool[0].VideoID != 4 {
			t.Fatalf("expected only hot route candidate, got %+v", pool)
		}
	})

	t.Run("无召回器回退单路热度池", func(t *testing.T) {
		repo := &fakeRepo{userVector: []float64{1, 0, 0}, hasVector: true}
		service := New(repo, WithNow(func() time.Time { return now }))
		pool, _, err := service.recallCandidates(context.Background(), 42, 50)
		if err != nil {
			t.Fatalf("fallback recall failed: %v", err)
		}
		if len(pool) != 0 {
			t.Fatalf("expected empty pool from fake fallback, got %d", len(pool))
		}
	})

	t.Run("曝光剔除在合并前生效", func(t *testing.T) {
		repo := &fakeRepo{userVector: []float64{1, 0, 0}, hasVector: true, exposed: map[int64]bool{1: true}}
		vector := &fakeRecaller{source: domainrecommendation.SourceVector, cands: []*domainrecommendation.Candidate{
			candidate(1, 10, 0.9, domainrecommendation.SourceVector),
			candidate(2, 20, 0.8, domainrecommendation.SourceVector),
		}}
		service := New(repo, WithRecallerSet([]domainrecommendation.Recaller{vector}), WithNow(func() time.Time { return now }))

		pool, _, err := service.recallCandidates(context.Background(), 42, 50)
		if err != nil {
			t.Fatalf("recall failed: %v", err)
		}
		if len(pool) != 1 || pool[0].VideoID != 2 {
			t.Fatalf("expected exposed video removed, got %+v", pool)
		}
	})
}
