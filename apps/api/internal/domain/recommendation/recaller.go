package domainrecommendation

import (
	"context"
	"time"
)

const (
	SourceHot       = "hot"
	SourceFollowing = "following"
	SourceVector    = "vector"
)

const (
	FollowingRecallQuotaRatio = 0.2
	FollowingRecallLimitCap   = 200
)

type RecallInput struct {
	UserID        int64
	Limit         int
	QueryVector   []float64
	HasUserVector bool
	Now           time.Time
}

type Recaller interface {
	Recall(ctx context.Context, in RecallInput) ([]*Candidate, error)
}

// Merger 合并多路召回结果：跨路去重、热度补齐、关注路配额保底、曝光剔除、截断。
type Merger struct {
	PoolLimit      int
	FollowingFloor float64
}

func NewMerger(poolLimit int, followingFloor float64) *Merger {
	if followingFloor <= 0 {
		followingFloor = FollowingRecallQuotaRatio
	}
	if followingFloor > 1 {
		followingFloor = 1
	}
	return &Merger{PoolLimit: poolLimit, FollowingFloor: followingFloor}
}

// Merge 接收各路召回结果与已曝光集合，返回合并后的候选池。
// 跨路去重按 vector > following > hot 的优先级保留首个来源，
// 重复候选的 HotScore 取多条中的最大值补齐，避免排名阶段热度分归零。
// 填充顺序：following 候选优先占满配额（防被低分 hot/vector 挤出 → 白召回），
// 再依次填 vector、hot 至 poolLimit。
func (m *Merger) Merge(routes map[string][]*Candidate, exposed map[int64]bool) []*Candidate {
	if m == nil || m.PoolLimit <= 0 {
		return []*Candidate{}
	}

	best := make(map[int64]*Candidate, 256)
	addRoute := func(source string) {
		for _, candidate := range routes[source] {
			if candidate == nil || candidate.VideoID <= 0 {
				continue
			}
			existing, ok := best[candidate.VideoID]
			if !ok {
				candidate.Source = source
				best[candidate.VideoID] = candidate
				continue
			}
			if candidate.HotScore > existing.HotScore {
				existing.HotScore = candidate.HotScore
			}
			if sourcePriority(source) < sourcePriority(existing.Source) {
				existing.Source = source
			}
		}
	}
	addRoute(SourceVector)
	addRoute(SourceFollowing)
	addRoute(SourceHot)

	quota := maxInt(int(m.FollowingFloor*float64(m.PoolLimit)), 1)
	result := make([]*Candidate, 0, m.PoolLimit)
	appendRoute := func(source string) {
		for _, candidate := range best {
			if candidate == nil || len(result) >= m.PoolLimit {
				continue
			}
			if exposed[candidate.VideoID] {
				continue
			}
			if candidate.Source == SourceFollowing && len(result) >= quota {
				continue
			}
			if candidate.Source != source {
				continue
			}
			result = append(result, candidate)
		}
	}
	appendRoute(SourceFollowing)
	appendRoute(SourceVector)
	appendRoute(SourceHot)
	return result
}

func sourcePriority(source string) int {
	switch source {
	case SourceVector:
		return 0
	case SourceFollowing:
		return 1
	default:
		return 2
	}
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
