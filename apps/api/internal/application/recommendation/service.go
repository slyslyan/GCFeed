package applicationrecommendation

import (
	domainrecommendation "GCFeed/internal/domain/recommendation"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

const defaultLimit = 10

// 固定候选窗口，使同一模型下不同页大小使用一致的相似度排序集合。
const maxCandidatePoolSize = 500
const recallTimeout = 500 * time.Millisecond

var ErrLoadRecommendationFailed = errors.New("failed to load recommendations")
var ErrLoadExposureDecisionsFailed = errors.New("failed to load exposure decisions")
var ErrSaveRecommendationExposureFailed = errors.New("failed to save recommendation exposure")

type CandidateRecaller interface {
	Recall(ctx context.Context, userID int64, limit int) ([]*domainrecommendation.Candidate, error)
}

type Service struct {
	repo     domainrecommendation.Repository
	recaller CandidateRecaller
	now      func() time.Time
}

type Option func(*Service)

// WithRecaller 注册双塔模型的单路向量召回，不回退到热榜或关注流。
func WithRecaller(recaller CandidateRecaller) Option {
	return func(s *Service) { s.recaller = recaller }
}

type CandidateRequest struct {
	UserID    int64
	Scene     string
	RequestID string
	Cursor    string
	Limit     int
}

type CandidateResult struct {
	UserID     int64
	Scene      string
	RequestID  string
	Candidates []*domainrecommendation.Candidate
	NextCursor string
	HasMore    bool
}

type ExposureInput struct {
	UserID    int64
	VideoID   int64
	Scene     string
	RequestID string
}

type ExposureDecisionInput struct {
	UserID    int64
	Scene     string
	RequestID string
	VideoIDs  []int64
}

type ExposureDecisionResult struct {
	UserID    int64
	Scene     string
	RequestID string
	Decisions []*domainrecommendation.ExposureDecision
}

type ExposureResult struct {
	Exposures []*domainrecommendation.Exposure
}

type cursorPayload struct {
	ModelVersion string  `json:"model_version"`
	RankScore    float64 `json:"rank_score"`
	PublishedAt  string  `json:"published_at"`
	VideoID      int64   `json:"video_id"`
}

func New(repo domainrecommendation.Repository, options ...Option) *Service {
	service := &Service{
		repo: repo,
		now:  func() time.Time { return time.Now().UTC().Truncate(time.Minute) },
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func WithNow(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

func (s *Service) Recommend(ctx context.Context, input CandidateRequest) (*CandidateResult, error) {
	limit := normalizeLimit(input.Limit)
	cursor, err := parseCursor(input.Cursor)
	if err != nil {
		return nil, err
	}
	req, err := domainrecommendation.NewCandidateRequest(input.UserID, input.Scene, input.RequestID, cursor, limit)
	if err != nil {
		return nil, err
	}

	ranked, err := s.recallCandidates(ctx, req.UserID, req.Cursor)
	if err != nil {
		if errors.Is(err, domainrecommendation.ErrModelUnavailable) || errors.Is(err, domainrecommendation.ErrInvalidCursor) {
			return nil, err
		}
		return nil, ErrLoadRecommendationFailed
	}
	ranked = filterByCursor(ranked, req.Cursor)

	hasMore := len(ranked) > limit
	if hasMore {
		ranked = ranked[:limit]
	}

	nextCursor := ""
	if len(ranked) > 0 {
		nextCursor = encodeCursor(&domainrecommendation.Cursor{
			ModelVersion: ranked[len(ranked)-1].ModelVersion,
			RankScore:    ranked[len(ranked)-1].RankScore,
			PublishedAt:  ranked[len(ranked)-1].PublishedAt,
			VideoID:      ranked[len(ranked)-1].VideoID,
		})
	}
	ranked = interleaveByAuthor(ranked)

	return &CandidateResult{
		UserID:     req.UserID,
		Scene:      req.Scene,
		RequestID:  req.RequestID,
		Candidates: ranked,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

func (s *Service) DecideExposures(ctx context.Context, input ExposureDecisionInput) (*ExposureDecisionResult, error) {
	req, err := domainrecommendation.NewExposureDecisionRequest(input.UserID, input.Scene, input.RequestID, input.VideoIDs)
	if err != nil {
		return nil, err
	}
	if len(req.VideoIDs) == 0 {
		return &ExposureDecisionResult{
			UserID:    req.UserID,
			Scene:     req.Scene,
			RequestID: req.RequestID,
			Decisions: []*domainrecommendation.ExposureDecision{},
		}, nil
	}

	exposures, err := s.repo.ListRecentExposures(ctx, req.UserID, req.VideoIDs, s.now().Add(-domainrecommendation.RecentExposureWindow))
	if err != nil {
		return nil, ErrLoadExposureDecisionsFailed
	}
	exposureByVideoID := make(map[int64]*domainrecommendation.Exposure, len(exposures))
	for _, exposure := range exposures {
		if exposure != nil {
			exposureByVideoID[exposure.VideoID] = exposure
		}
	}

	decisions := make([]*domainrecommendation.ExposureDecision, 0, len(req.VideoIDs))
	for _, videoID := range req.VideoIDs {
		if exposure := exposureByVideoID[videoID]; exposure != nil {
			decisions = append(decisions, domainrecommendation.RestoreExposureDecision(
				videoID,
				false,
				domainrecommendation.ExposureDecisionReasonRecentlyExposed,
				&exposure.LastExposedAt,
			))
			continue
		}
		decisions = append(decisions, domainrecommendation.RestoreExposureDecision(
			videoID,
			true,
			domainrecommendation.ExposureDecisionReasonFresh,
			nil,
		))
	}
	return &ExposureDecisionResult{
		UserID:    req.UserID,
		Scene:     req.Scene,
		RequestID: req.RequestID,
		Decisions: decisions,
	}, nil
}

func (s *Service) SaveExposures(ctx context.Context, inputs []ExposureInput) (*ExposureResult, error) {
	writes := make([]*domainrecommendation.ExposureWrite, 0, len(inputs))
	seen := map[int64]struct{}{}
	for _, input := range inputs {
		write, err := domainrecommendation.NewExposureWrite(input.UserID, input.VideoID, input.Scene, input.RequestID)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[write.VideoID]; exists {
			continue
		}
		seen[write.VideoID] = struct{}{}
		writes = append(writes, write)
	}
	if len(writes) == 0 {
		return &ExposureResult{Exposures: []*domainrecommendation.Exposure{}}, nil
	}

	exposures, err := s.repo.SaveExposures(ctx, writes)
	if err != nil {
		if errors.Is(err, domainrecommendation.ErrVideoNotFound) {
			return nil, err
		}
		return nil, ErrSaveRecommendationExposureFailed
	}
	return &ExposureResult{Exposures: exposures}, nil
}

// recallCandidates 只使用模型召回分数；曝光过滤和去重不改变候选相似度。
func (s *Service) recallCandidates(ctx context.Context, userID int64, cursor *domainrecommendation.Cursor) ([]*domainrecommendation.Candidate, error) {
	if s.recaller == nil {
		return nil, domainrecommendation.ErrModelUnavailable
	}
	recallCtx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()
	pool, err := s.recaller.Recall(recallCtx, userID, maxCandidatePoolSize)
	if err != nil {
		return nil, err
	}
	if err := recallCtx.Err(); err != nil {
		return nil, err
	}

	ranked := make([]*domainrecommendation.Candidate, 0, len(pool))
	modelVersion := ""
	for _, candidate := range pool {
		if candidate == nil || candidate.VideoID <= 0 || candidate.AuthorID <= 0 || candidate.PublishedAt.IsZero() ||
			math.IsNaN(candidate.Similarity) || math.IsInf(candidate.Similarity, 0) || candidate.Similarity < -1 || candidate.Similarity > 1 {
			continue
		}
		if candidate.Source != domainrecommendation.SourceVector {
			continue
		}
		version := strings.TrimSpace(candidate.ModelVersion)
		if version == "" || len(version) > domainrecommendation.MaxModelVersionLength {
			return nil, domainrecommendation.ErrModelUnavailable
		}
		if modelVersion != "" && modelVersion != version {
			return nil, domainrecommendation.ErrModelUnavailable
		}
		modelVersion = version
		value := *candidate
		value.ModelVersion = version
		value.RankScore = value.Similarity
		value.HotScore = 0
		value.FreshnessScore = 0
		if value.Reason != "cold_start" {
			value.Reason = "interest_match"
		}
		ranked = append(ranked, &value)
	}
	if cursor != nil && modelVersion != "" && cursor.ModelVersion != modelVersion {
		return nil, domainrecommendation.ErrInvalidCursor
	}
	sortCandidates(ranked)
	seen := make(map[int64]bool, len(ranked))
	videoIDs := make([]int64, 0, len(ranked))
	deduped := make([]*domainrecommendation.Candidate, 0, len(ranked))
	for _, candidate := range ranked {
		if seen[candidate.VideoID] {
			continue
		}
		seen[candidate.VideoID] = true
		videoIDs = append(videoIDs, candidate.VideoID)
		deduped = append(deduped, candidate)
	}
	if len(videoIDs) == 0 {
		return deduped, nil
	}
	exposures, err := s.repo.ListRecentExposures(ctx, userID, videoIDs, s.now().Add(-domainrecommendation.RecentExposureWindow))
	if err != nil {
		return nil, err
	}
	exposed := make(map[int64]bool, len(exposures))
	for _, exposure := range exposures {
		if exposure != nil {
			exposed[exposure.VideoID] = true
		}
	}
	result := make([]*domainrecommendation.Candidate, 0, len(deduped))
	for _, candidate := range deduped {
		if !exposed[candidate.VideoID] {
			result = append(result, candidate)
			if len(result) == maxCandidatePoolSize {
				break
			}
		}
	}
	return result, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > domainrecommendation.MaxLimit {
		return domainrecommendation.MaxLimit
	}
	return limit
}

func sortCandidates(candidates []*domainrecommendation.Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.RankScore != right.RankScore {
			return left.RankScore > right.RankScore
		}
		if !left.PublishedAt.Equal(right.PublishedAt) {
			return left.PublishedAt.After(right.PublishedAt)
		}
		return left.VideoID > right.VideoID
	})
}

func interleaveByAuthor(candidates []*domainrecommendation.Candidate) []*domainrecommendation.Candidate {
	if len(candidates) <= 2 {
		return candidates
	}
	output := make([]*domainrecommendation.Candidate, 0, len(candidates))
	delayed := make([]*domainrecommendation.Candidate, 0)
	var previousAuthorID int64
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if previousAuthorID != 0 && candidate.AuthorID == previousAuthorID {
			delayed = append(delayed, candidate)
			continue
		}
		output = append(output, candidate)
		previousAuthorID = candidate.AuthorID
	}
	for len(delayed) > 0 {
		progressed := false
		remaining := make([]*domainrecommendation.Candidate, 0)
		for _, candidate := range delayed {
			if len(output) > 0 && output[len(output)-1].AuthorID == candidate.AuthorID {
				remaining = append(remaining, candidate)
				continue
			}
			output = append(output, candidate)
			progressed = true
		}
		if !progressed {
			output = append(output, remaining...)
			break
		}
		delayed = remaining
	}
	return output
}

func filterByCursor(candidates []*domainrecommendation.Candidate, cursor *domainrecommendation.Cursor) []*domainrecommendation.Candidate {
	if cursor == nil {
		return candidates
	}
	filtered := make([]*domainrecommendation.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if candidate.RankScore < cursor.RankScore ||
			(candidate.RankScore == cursor.RankScore && candidate.PublishedAt.Before(cursor.PublishedAt)) ||
			(candidate.RankScore == cursor.RankScore && candidate.PublishedAt.Equal(cursor.PublishedAt) && candidate.VideoID < cursor.VideoID) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func parseCursor(raw string) (*domainrecommendation.Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	content, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		content, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, domainrecommendation.ErrInvalidCursor
		}
	}
	var payload cursorPayload
	if err := json.Unmarshal(content, &payload); err != nil {
		return nil, domainrecommendation.ErrInvalidCursor
	}
	publishedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(payload.PublishedAt))
	if err != nil {
		return nil, domainrecommendation.ErrInvalidCursor
	}
	cursor := &domainrecommendation.Cursor{
		ModelVersion: payload.ModelVersion,
		RankScore:    payload.RankScore,
		PublishedAt:  publishedAt,
		VideoID:      payload.VideoID,
	}
	if !cursor.Valid() {
		return nil, domainrecommendation.ErrInvalidCursor
	}
	return cursor, nil
}

func encodeCursor(cursor *domainrecommendation.Cursor) string {
	if cursor == nil || !cursor.Valid() {
		return ""
	}
	content, err := json.Marshal(cursorPayload{
		ModelVersion: cursor.ModelVersion,
		RankScore:    cursor.RankScore,
		PublishedAt:  cursor.PublishedAt.UTC().Format(time.RFC3339Nano),
		VideoID:      cursor.VideoID,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(content)
}
