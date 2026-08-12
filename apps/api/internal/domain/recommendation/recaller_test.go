package domainrecommendation

import (
	"testing"
	"time"
)

func TestMerger_Merge(t *testing.T) {
	now := time.Now()
	video := func(id int64, authorID int64, hot int) *Candidate {
		return RestoreCandidate(id, authorID, 0, 0, hot, 0, "", now)
	}

	t.Run("去重按 vector > following > hot 保留来源并补齐 HotScore", func(t *testing.T) {
		merger := NewMerger(50, 0)
		routes := map[string][]*Candidate{
			SourceHot:       {video(1, 10, 100)},
			SourceFollowing: {video(1, 10, 50)},
			SourceVector:    {video(1, 10, 10)},
		}
		pool := merger.Merge(routes, nil)
		if len(pool) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(pool))
		}
		if pool[0].Source != SourceVector {
			t.Errorf("expected source vector, got %s", pool[0].Source)
		}
		if pool[0].HotScore != 100 {
			t.Errorf("expected hot score merged to max 100, got %d", pool[0].HotScore)
		}
	})

	t.Run("following 配额保底不被挤出", func(t *testing.T) {
		merger := NewMerger(10, FollowingRecallQuotaRatio)
		var following []*Candidate
		for i := int64(1); i <= 3; i++ {
			following = append(following, video(i, 100, 1))
		}
		var hot []*Candidate
		for i := int64(11); i <= 20; i++ {
			hot = append(hot, video(i, 200, 10))
		}
		pool := merger.Merge(map[string][]*Candidate{
			SourceFollowing: following,
			SourceHot:       hot,
		}, nil)
		if len(pool) != 10 {
			t.Fatalf("expected pool size 10, got %d", len(pool))
		}
		followingCount := 0
		for _, candidate := range pool {
			if candidate.Source == SourceFollowing {
				followingCount++
			}
		}
		// 配额 = floor(0.2 × 10) = 2：高分 hot 挤不掉配额内的 following，但第 3 条被截断。
		if followingCount != 2 {
			t.Errorf("expected following quota 2, got %d", followingCount)
		}
	})

	t.Run("following 超过配额时截断", func(t *testing.T) {
		merger := NewMerger(10, 0.2)
		var following []*Candidate
		for i := int64(1); i <= 10; i++ {
			following = append(following, video(i, 100, 1))
		}
		pool := merger.Merge(map[string][]*Candidate{
			SourceFollowing: following,
		}, nil)
		if len(pool) != 2 {
			t.Fatalf("expected quota 2 following candidates, got %d", len(pool))
		}
	})

	t.Run("曝光剔除", func(t *testing.T) {
		merger := NewMerger(10, 0)
		exposed := map[int64]bool{2: true, 3: true}
		pool := merger.Merge(map[string][]*Candidate{
			SourceHot: {video(1, 10, 5), video(2, 10, 5), video(3, 10, 5)},
		}, exposed)
		if len(pool) != 1 || pool[0].VideoID != 1 {
			t.Fatalf("expected only video 1, got %+v", pool)
		}
	})

	t.Run("空路与空池", func(t *testing.T) {
		merger := NewMerger(50, 0)
		if pool := merger.Merge(nil, nil); len(pool) != 0 {
			t.Fatalf("expected empty pool, got %d", len(pool))
		}
		if pool := merger.Merge(map[string][]*Candidate{
			SourceVector:    {},
			SourceFollowing: {},
			SourceHot:       {},
		}, nil); len(pool) != 0 {
			t.Fatalf("expected empty pool, got %d", len(pool))
		}
	})

	t.Run("截断到 poolLimit", func(t *testing.T) {
		merger := NewMerger(3, 0)
		var hot []*Candidate
		for i := int64(1); i <= 10; i++ {
			hot = append(hot, video(i, 10, int(i)))
		}
		pool := merger.Merge(map[string][]*Candidate{SourceHot: hot}, nil)
		if len(pool) != 3 {
			t.Fatalf("expected 3 candidates, got %d", len(pool))
		}
	})
}
