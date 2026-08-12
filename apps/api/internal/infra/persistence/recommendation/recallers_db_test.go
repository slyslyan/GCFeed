package infrarecommendation

import (
	infravector "GCFeed/internal/infra/vector"
	"context"
	"os"
	"testing"
	"time"
)

// gated Milvus 集成测试：TEST_MILVUS_ADDR 未设置时跳过（本地 docker Milvus standalone 验证 ANN 链路）。
func newMilvusStoreForTest(t *testing.T) *infravector.MilvusStore {
	t.Helper()
	addr := os.Getenv("TEST_MILVUS_ADDR")
	if addr == "" {
		t.Skip("TEST_MILVUS_ADDR not set, skip Milvus integration test")
	}
	store, err := infravector.NewMilvusStore(addr)
	if err != nil {
		t.Fatalf("connect milvus: %v", err)
	}
	return store
}

func TestMilvusANN(t *testing.T) {
	store := newMilvusStoreForTest(t)
	defer store.Close()
	ctx := context.Background()

	if err := store.EnsureReady(ctx); err != nil {
		t.Fatalf("first EnsureReady failed: %v", err)
	}
	if err := store.EnsureReady(ctx); err != nil {
		t.Fatalf("second EnsureReady not idempotent: %v", err)
	}

	// 3 条 128 维 one-hot 风格向量，便于断言距离顺序。
	vectors := []infravector.VideoVector{
		{VideoID: 1, Vector: oneHot(128, 0)},
		{VideoID: 2, Vector: oneHot(128, 3)},
		{VideoID: 3, Vector: oneHot(128, 127)},
	}
	if err := store.UpsertVideos(ctx, vectors); err != nil {
		t.Fatalf("upsert vectors: %v", err)
	}
	// 等索引对新数据重建完成，否则检索结果不完整。
	if err := store.WaitIndexReady(ctx, 30*time.Second); err != nil {
		t.Fatalf("wait index ready: %v", err)
	}

	query := oneHot(128, 0) // 与视频 1 完全相同（相似度 1），与 2/3 正交（相似度 0）

	hits, err := store.Search(ctx, query, 3)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}
	if hits[0].VideoID != 1 {
		t.Errorf("expected nearest video 1, got %d", hits[0].VideoID)
	}
	// 视频 2/3 与查询正交、分数同为 0，相对顺序不定，只断言集合。
	tail := map[int64]bool{hits[1].VideoID: true, hits[2].VideoID: true}
	if !tail[2] || !tail[3] {
		t.Errorf("expected videos 2 and 3 in tail, got %+v", hits)
	}
	if !(hits[0].Distance >= hits[1].Distance && hits[1].Distance >= hits[2].Distance) {
		t.Errorf("scores not descending: %+v", hits)
	}
	// 钉死 COSINE 分数语义：实测 v2.4.13 返回余弦相似度（相同向量得 1，正交得 0）。
	if got := hits[0].Distance; got < 0.999 {
		t.Errorf("expected identical video score ~1, got %v", got)
	}
	if got := hits[2].Distance; got > 0.001 {
		t.Errorf("expected orthogonal video score ~0, got %v", got)
	}
	t.Logf("hits: %+v", hits)
}

func oneHot(dimension int, index int) []float64 {
	vec := make([]float64, dimension)
	vec[index] = 1
	return vec
}
