package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	feed "GCFeed/internal/application/feed"
	recommendation "GCFeed/internal/application/recommendation"
	domain "GCFeed/internal/domain/recommendation"
	twotower "GCFeed/internal/infra/twotower"
)

type fixtureCatalog struct{}

func (fixtureCatalog) PublishedCandidates(ctx context.Context, ids []int64) (map[int64]*domain.Candidate, error) {
	cards := make(map[int64]*domain.Candidate)
	for _, item := range seedFeedItems() {
		cards[item.VideoID] = &domain.Candidate{VideoID: item.VideoID, AuthorID: item.AuthorID, PublishedAt: item.PublishedAt, Source: domain.SourceVector}
	}
	return cards, nil
}

func TestRecommendSnapshotTransition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	page := func(userID int64, cursor string) (feedAPIResponse, int) {
		r, err := twotower.Open(context.Background(), path, "", fixtureCatalog{})
		if err != nil {
			t.Fatal(err)
		}
		svc := recommendation.New(newMemoryRecommendationRepo(), recommendation.WithRecaller(r))
		router, jwt := newFeedRouterWithServiceAndJWT(t, feed.New(newMemoryFeedRepo(seedFeedItems()), feed.WithRecommender(svc)))
		token := ""
		if userID != 0 {
			token = signTestToken(t, jwt, userID)
		}
		body := fmt.Sprintf(`{"scene":"recommend","limit":1,"cursor":%q,"context":{"request_id":"web-test"}}`, cursor)
		resp := performJSONRequest(router, http.MethodPost, "/api/feed-queries", body, token)
		var p feedAPIResponse
		if resp.Code == 200 {
			decodeJSON(t, resp, &p)
		}
		return p, resp.Code
	}
	latest, status := page(42, "")
	if status != 200 || latest.Scene != "recommend" || len(latest.Items) != 1 || latest.Items[0].VideoID != 3 || !latest.HasMore {
		t.Fatalf("missing snapshot first page: %d %+v", status, latest)
	}
	if _, status := page(0, latest.NextCursor); status != 401 {
		t.Fatalf("unauthenticated continuation: %d", status)
	}
	v1, v2 := make([]float64, 128), make([]float64, 128)
	v1[0], v2[1] = 1, 1
	data, err := json.Marshal(map[string]any{"version": "test", "dimension": 128, "default_user": v2, "users": map[string]any{"42": v1}, "videos": map[string]any{"1": v1, "2": v2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	next, status := page(42, latest.NextCursor)
	if status != 200 || len(next.Items) != 1 || next.Items[0].VideoID != 2 || !next.HasMore {
		t.Fatalf("changed order during fallback: %d %+v", status, next)
	}
	reco, status := page(42, "")
	if status != 200 || len(reco.Items) != 1 || reco.Items[0].VideoID != 1 {
		t.Fatalf("expected personalized first page: %d %+v", status, reco)
	}
	cold, status := page(99, "")
	if status != 200 || len(cold.Items) != 1 || cold.Items[0].VideoID != 2 {
		t.Fatalf("expected model's cold-start vector: %d %+v", status, cold)
	}
	// Losing the model between two personalized pages must not splice in timeline items.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if _, status := page(42, reco.NextCursor); status != 503 {
		t.Fatalf("missing snapshot on model continuation: %d", status)
	}

	// A valid snapshot whose only hit is no longer published has no eligible videos.
	data, err = json.Marshal(map[string]any{"version": "removed", "dimension": 128, "default_user": v1, "videos": map[string]any{"100": v1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	empty, status := page(42, "")
	if status != 200 || len(empty.Items) != 0 || empty.HasMore {
		t.Fatalf("empty model result unexpectedly fell back: %d %+v", status, empty)
	}
}
