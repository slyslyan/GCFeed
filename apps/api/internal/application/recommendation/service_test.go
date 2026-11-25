package applicationrecommendation

import (
	domainrecommendation "GCFeed/internal/domain/recommendation"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

type fakeRepo struct {
	exposed map[int64]bool
	err     error
}

func (f *fakeRepo) ListRecentExposures(ctx context.Context, userID int64, videoIDs []int64, since time.Time) ([]*domainrecommendation.Exposure, error) {
	if f.err != nil {
		return nil, f.err
	}
	exposures := make([]*domainrecommendation.Exposure, 0)
	for _, id := range videoIDs {
		if f.exposed[id] {
			exposures = append(exposures, domainrecommendation.RestoreExposure(1, userID, id, since, since, 1, "recommend"))
		}
	}
	return exposures, nil
}
func (f *fakeRepo) SaveExposures(ctx context.Context, writes []*domainrecommendation.ExposureWrite) ([]*domainrecommendation.Exposure, error) {
	return []*domainrecommendation.Exposure{}, nil
}

type fakeRecaller struct {
	candidates    []*domainrecommendation.Candidate
	err           error
	waitForCancel bool
	limit         int
}

func (f *fakeRecaller) Recall(ctx context.Context, userID int64, limit int) ([]*domainrecommendation.Candidate, error) {
	f.limit = limit
	if f.waitForCancel {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.candidates, f.err
}
func candidate(id, authorID int64, similarity float64) *domainrecommendation.Candidate {
	return &domainrecommendation.Candidate{
		ModelVersion: "two-tower-v1", VideoID: id, AuthorID: authorID,
		Similarity: similarity, PublishedAt: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
		Source: domainrecommendation.SourceVector,
	}
}
func candidateIDs(items []*domainrecommendation.Candidate) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.VideoID)
	}
	return ids
}
func request(limit int, cursor string) CandidateRequest {
	return CandidateRequest{UserID: 42, Scene: "recommend", Limit: limit, Cursor: cursor}
}

func TestRecommendUsesOnlyModelSimilarity(t *testing.T) {
	first := candidate(1, 10, .8)
	second := candidate(2, 20, .1)
	second.RankScore, second.HotScore, second.FreshnessScore = 100, 100000, 100
	hot := candidate(3, 30, 1)
	hot.Source = "hot"
	following := candidate(4, 40, 1)
	following.Source = "following"
	recaller := &fakeRecaller{candidates: []*domainrecommendation.Candidate{second, hot, first, following}}
	service := New(&fakeRepo{}, WithRecaller(recaller))
	result, err := service.Recommend(context.Background(), request(10, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(result.Candidates); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("unexpected vector order: %v", got)
	}
	for _, item := range result.Candidates {
		if item.RankScore != item.Similarity || item.HotScore != 0 || item.FreshnessScore != 0 {
			t.Fatalf("handcrafted score survived: %+v", item)
		}
	}
	if second.RankScore != 100 || second.HotScore != 100000 {
		t.Fatal("recaller-owned candidate mutated")
	}
	if recaller.limit != maxCandidatePoolSize {
		t.Fatalf("unexpected recall window: %d", recaller.limit)
	}
}

func TestRecommendFiltersInvalidExposedAndDuplicateCandidates(t *testing.T) {
	badScore := candidate(9, 90, math.NaN())
	badRange := candidate(10, 100, 1.1)
	badID := candidate(0, 1, .9)
	badAuthor := candidate(11, 0, .9)
	recaller := &fakeRecaller{candidates: []*domainrecommendation.Candidate{
		nil, candidate(1, 10, .3), candidate(1, 10, .9), candidate(2, 20, .8), candidate(3, 30, -.4),
		badScore, badRange, badID, badAuthor,
	}}
	result, err := New(&fakeRepo{exposed: map[int64]bool{2: true}}, WithRecaller(recaller)).Recommend(context.Background(), request(10, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(result.Candidates); !reflect.DeepEqual(got, []int64{1, 3}) {
		t.Fatalf("unexpected filtering: %v", got)
	}
	if result.Candidates[0].Similarity != .9 || result.Candidates[1].Similarity != -.4 {
		t.Fatalf("scores changed: %+v", result.Candidates)
	}
}

func TestRecommendPaginationUsesExactSimilarityAndStableTies(t *testing.T) {
	// 小于旧 epsilon 的分差仍是不同分数，不能凭发布时间跨页重复。
	first := candidate(1, 10, .8)
	first.PublishedAt = first.PublishedAt.Add(-time.Hour)
	second := candidate(2, 20, .8-1e-10)
	third := candidate(4, 30, -.2)
	fourth := candidate(3, 40, -.2)
	recaller := &fakeRecaller{candidates: []*domainrecommendation.Candidate{fourth, second, third, first}}
	service := New(&fakeRepo{}, WithRecaller(recaller))
	page, err := service.Recommend(context.Background(), request(1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.Candidates[0].VideoID != 1 {
		t.Fatalf("unexpected first page: %+v", page)
	}
	page, err = service.Recommend(context.Background(), request(2, page.NextCursor))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(page.Candidates); !reflect.DeepEqual(got, []int64{2, 4}) || !page.HasMore {
		t.Fatalf("unexpected second page: %+v", page)
	}
	page, err = service.Recommend(context.Background(), request(2, page.NextCursor))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(page.Candidates); !reflect.DeepEqual(got, []int64{3}) || page.HasMore {
		t.Fatalf("unexpected last page: %+v", page)
	}
}

func TestRecommendCursorUsesBoundaryBeforeAuthorInterleaving(t *testing.T) {
	recaller := &fakeRecaller{candidates: []*domainrecommendation.Candidate{
		candidate(1, 10, .9), candidate(2, 10, .8), candidate(3, 20, .7), candidate(4, 30, .6),
	}}
	service := New(&fakeRepo{}, WithRecaller(recaller))
	page, err := service.Recommend(context.Background(), request(3, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(page.Candidates); !reflect.DeepEqual(got, []int64{1, 3, 2}) {
		t.Fatalf("author interleaving lost: %v", got)
	}
	cursor, err := parseCursor(page.NextCursor)
	if err != nil || cursor.VideoID != 3 {
		t.Fatalf("cursor uses display position: %+v, %v", cursor, err)
	}
	next, err := service.Recommend(context.Background(), request(3, page.NextCursor))
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(next.Candidates); !reflect.DeepEqual(got, []int64{4}) {
		t.Fatalf("pagination repeated/skipped items: %v", got)
	}
}

func TestRecommendBoundsOversamplingAfterExposureFilter(t *testing.T) {
	recaller := &fakeRecaller{}
	repo := &fakeRepo{exposed: map[int64]bool{}}
	for id := int64(1); id <= maxCandidatePoolSize+30; id++ {
		recaller.candidates = append(recaller.candidates, candidate(id, id, 1-float64(id)/1000))
		if id <= 10 {
			repo.exposed[id] = true
		}
	}
	service := New(repo, WithRecaller(recaller))
	cursor := ""
	seen := map[int64]bool{}
	for pageNumber := 0; pageNumber < 5; pageNumber++ {
		page, err := service.Recommend(context.Background(), request(100, cursor))
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Candidates) != 100 || page.HasMore != (pageNumber < 4) {
			t.Fatalf("unexpected bounded page %d: size=%d has_more=%v", pageNumber, len(page.Candidates), page.HasMore)
		}
		for _, item := range page.Candidates {
			if seen[item.VideoID] || item.VideoID <= 10 || item.VideoID > maxCandidatePoolSize+10 {
				t.Fatalf("oversampling boundary skipped exposure filtering or repeated a video: %+v", item)
			}
			seen[item.VideoID] = true
		}
		cursor = page.NextCursor
	}
	if len(seen) != maxCandidatePoolSize || !seen[maxCandidatePoolSize+10] {
		t.Fatalf("window was capped before exposure filtering: size=%d", len(seen))
	}
	page, err := service.Recommend(context.Background(), request(100, cursor))
	if err != nil || len(page.Candidates) != 0 || page.HasMore {
		t.Fatalf("continuation escaped the fixed window: %+v, %v", page, err)
	}
}

func TestRecommendColdStartUsesModelCandidates(t *testing.T) {
	cold := candidate(1, 10, -.2)
	cold.Reason = "cold_start"
	result, err := New(&fakeRepo{}, WithRecaller(&fakeRecaller{candidates: []*domainrecommendation.Candidate{cold}})).Recommend(context.Background(), request(10, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].Reason != "cold_start" || result.Candidates[0].RankScore != -.2 {
		t.Fatalf("cold model candidate changed: %+v", result)
	}
}

func TestRecommendPreservesModelErrorsAndHasNoHotFallback(t *testing.T) {
	t.Run("missing recaller", func(t *testing.T) {
		_, err := New(&fakeRepo{}).Recommend(context.Background(), request(10, ""))
		if !errors.Is(err, domainrecommendation.ErrModelUnavailable) {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("wrapped unavailable error", func(t *testing.T) {
		want := fmt.Errorf("load snapshot: %w", domainrecommendation.ErrModelUnavailable)
		_, err := New(&fakeRepo{}, WithRecaller(&fakeRecaller{err: want})).Recommend(context.Background(), request(10, ""))
		if err != want {
			t.Fatalf("model failure lost: %v", err)
		}
	})
	t.Run("inference failure", func(t *testing.T) {
		_, err := New(&fakeRepo{}, WithRecaller(&fakeRecaller{err: errors.New("ann failed")})).Recommend(context.Background(), request(10, ""))
		if !errors.Is(err, ErrLoadRecommendationFailed) {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := New(&fakeRepo{}, WithRecaller(&fakeRecaller{waitForCancel: true})).Recommend(ctx, request(10, ""))
		if !errors.Is(err, ErrLoadRecommendationFailed) {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("exposure lookup failure", func(t *testing.T) {
		_, err := New(&fakeRepo{err: errors.New("database failed")}, WithRecaller(&fakeRecaller{candidates: []*domainrecommendation.Candidate{candidate(1, 10, .5)}})).Recommend(context.Background(), request(10, ""))
		if !errors.Is(err, ErrLoadRecommendationFailed) {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestRecommendRejectsCursorAfterModelSwitch(t *testing.T) {
	recaller := &fakeRecaller{candidates: []*domainrecommendation.Candidate{candidate(1, 10, .5), candidate(2, 20, .4)}}
	service := New(&fakeRepo{}, WithRecaller(recaller))
	page, err := service.Recommend(context.Background(), request(1, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range recaller.candidates {
		item.ModelVersion = "two-tower-v2"
	}
	_, err = service.Recommend(context.Background(), request(1, page.NextCursor))
	if !errors.Is(err, domainrecommendation.ErrInvalidCursor) {
		t.Fatalf("old model cursor accepted: %v", err)
	}
}

func TestRecommendRejectsMixedModelCandidates(t *testing.T) {
	one, two := candidate(1, 10, .5), candidate(2, 20, .4)
	two.ModelVersion = "two-tower-v2"
	_, err := New(&fakeRepo{}, WithRecaller(&fakeRecaller{candidates: []*domainrecommendation.Candidate{one, two}})).Recommend(context.Background(), request(10, ""))
	if !errors.Is(err, domainrecommendation.ErrModelUnavailable) {
		t.Fatalf("mixed models accepted: %v", err)
	}
}
