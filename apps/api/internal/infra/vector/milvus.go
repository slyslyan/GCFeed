package infravector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	commonpb "github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

const (
	VectorDimension = 128
	primaryField    = "video_id"
	vectorField     = "embedding"
	indexName       = "embedding_hnsw"
)

type VideoVector struct {
	VideoID int64
	Vector  []float64
}

type VectorHit struct {
	VideoID  int64
	Distance float64
}

// VectorStore 向量检索能力抽象，便于测试替换与未来切换后端。
type VectorStore interface {
	EnsureReady(ctx context.Context) error
	UpsertVideos(ctx context.Context, vectors []VideoVector) error
	Search(ctx context.Context, query []float64, topK int) ([]VectorHit, error)
	Close()
}

type MilvusStore struct {
	client     client.Client
	collection string
}

// NewSnapshotStore isolates the index by the hash of the entire model snapshot.
func NewSnapshotStore(ctx context.Context, address string, digest [sha256.Size]byte) (*MilvusStore, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := client.NewClient(ctx, client.Config{Address: address})
	if err != nil {
		return nil, err
	}
	return &MilvusStore{client: c, collection: fmt.Sprintf("gcfeed_twotower_%x", digest)}, nil
}

// EnsureReady 幂等：创建 collection（不存在时）、建索引、加载到内存。
func (s *MilvusStore) EnsureReady(ctx context.Context) error {
	exists, err := s.client.HasCollection(ctx, s.collection)
	if err != nil {
		return err
	}
	if !exists {
		schema := entity.NewSchema().
			WithName(s.collection).
			WithDescription("GCFeed video embeddings").
			WithField(entity.NewField().WithName(primaryField).WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true).WithIsAutoID(false)).
			WithField(entity.NewField().WithName(vectorField).WithDataType(entity.FieldTypeFloatVector).WithDim(VectorDimension))
		if err := s.client.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
			return err
		}
		log.Printf("created milvus collection %s", s.collection)
	}

	// Versioned collections have a fixed schema and never replace a live index.
	indexes, err := s.client.DescribeIndex(ctx, s.collection, vectorField)
	if err != nil && !isIndexNotFound(err) {
		return err
	}
	if len(indexes) == 0 {
		index, err := entity.NewIndexHNSW(entity.COSINE, 16, 200)
		if err != nil {
			return err
		}
		if err := s.client.CreateIndex(ctx, s.collection, vectorField, index, false, client.WithIndexName(indexName)); err != nil {
			return err
		}
		log.Printf("created milvus vector index %s", indexName)
	}
	return s.client.LoadCollection(ctx, s.collection, false)
}

func isIndexNotFound(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "index not found")
}

// WaitIndexReady 轮询索引构建状态直至 Finished；upsert 新数据后等索引重建完成，检索结果才完整。
func (s *MilvusStore) WaitIndexReady(ctx context.Context, timeout time.Duration) error {
	if s == nil || s.client == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		state, err := s.client.GetIndexState(ctx, s.collection, vectorField, client.WithIndexName(indexName))
		if err != nil {
			if strings.HasPrefix(err.Error(), "index not found") {
				return errors.New("milvus index not found")
			}
			return err
		}
		if state == entity.IndexState(commonpb.IndexState_Finished) {
			return nil
		}
		if state == entity.IndexState(commonpb.IndexState_Failed) {
			return errors.New("milvus index build failed")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("milvus index build timeout, state: %v", state)
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// UpsertVideos 批量写入向量；Milvus 内部按主键去重。
func (s *MilvusStore) UpsertVideos(ctx context.Context, vectors []VideoVector) error {
	if len(vectors) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(vectors))
	floats := make([][]float32, 0, len(vectors))
	for _, v := range vectors {
		ids = append(ids, v.VideoID)
		floats = append(floats, toFloat32(v.Vector))
	}
	_, err := s.client.Upsert(ctx, s.collection, "",
		entity.NewColumnInt64(primaryField, ids),
		entity.NewColumnFloatVector(vectorField, VectorDimension, floats),
	)
	return err
}

// Search 按用户兴趣向量召回 topK 相似视频。
// 注意：实测 Milvus v2.4.13 对 COSINE 度量返回余弦相似度（相同向量为 1），
// 分数语义由 gated 测试 TestMilvusANN 钉死，升级 Milvus 版本后需回归确认。
func (s *MilvusStore) Search(ctx context.Context, query []float64, topK int) ([]VectorHit, error) {
	if topK <= 0 {
		return []VectorHit{}, nil
	}
	param, err := entity.NewIndexHNSWSearchParam(max(64, topK))
	if err != nil {
		return nil, err
	}
	results, err := s.client.Search(ctx, s.collection, []string{}, "",
		[]string{primaryField},
		[]entity.Vector{entity.FloatVector(toFloat32(query))},
		vectorField, entity.COSINE, topK, param, client.WithSearchQueryConsistencyLevel(entity.ClStrong),
	)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return []VectorHit{}, nil
	}
	result := results[0]
	if result.Err != nil {
		return nil, result.Err
	}
	if result.ResultCount == 0 {
		return nil, nil
	}
	ids, ok := result.IDs.(*entity.ColumnInt64)
	if !ok || ids.Len() != result.ResultCount || len(result.Scores) != result.ResultCount {
		return nil, errors.New("invalid Milvus result dimensions")
	}
	hits := make([]VectorHit, result.ResultCount)
	for i, id := range ids.Data() {
		hits[i] = VectorHit{VideoID: id, Distance: float64(result.Scores[i])}
	}
	return hits, nil
}

func (s *MilvusStore) Close() {
	if s != nil && s.client != nil {
		s.client.Close()
	}
}

func toFloat32(vector []float64) []float32 {
	out := make([]float32, len(vector))
	for i, value := range vector {
		out[i] = float32(value)
	}
	return out
}
