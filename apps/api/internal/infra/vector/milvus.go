package infravector

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	commonpb "github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

var ErrVectorStoreNotReady = errors.New("vector store not ready yet")

const (
	CollectionName  = "video_embedding"
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
	client client.Client
}

func NewMilvusStore(address string) (*MilvusStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := client.NewClient(ctx, client.Config{Address: address})
	if err != nil {
		return nil, err
	}
	return &MilvusStore{client: c}, nil
}

// StartMilvusStore 后台阻塞式重试连接，直至成功或 ctx 取消；调用方应放在 goroutine 中执行。
// Milvus 故障不阻塞服务启动，连接恢复前向量路自动降级。
func StartMilvusStore(ctx context.Context, address string, retryInterval time.Duration) *MilvusStore {
	for {
		store, err := NewMilvusStore(address)
		if err == nil {
			return store
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("milvus connect failed: %v, retrying in %s", err, retryInterval)
		select {
		case <-time.After(retryInterval):
		case <-ctx.Done():
			return nil
		}
	}
}

// EnsureReady 幂等：创建 collection（不存在时）、建索引、加载到内存。
func (s *MilvusStore) EnsureReady(ctx context.Context) error {
	exists, err := s.client.HasCollection(ctx, CollectionName)
	if err != nil {
		return err
	}
	if !exists {
		schema := entity.NewSchema().
			WithName(CollectionName).
			WithDescription("video hash-ngram embeddings").
			WithField(entity.NewField().WithName(primaryField).WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true).WithIsAutoID(false)).
			WithField(entity.NewField().WithName(vectorField).WithDataType(entity.FieldTypeFloatVector).WithDim(VectorDimension))
		if err := s.client.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
			return err
		}
		log.Printf("created milvus collection %s", CollectionName)
	}

	// 自愈式建索引：字段上已有目标索引则跳过；有其他名字的旧索引先 drop
	// （Milvus 同一字段不允许并存多个索引，且未命名索引默认以字段名命名）。
	indexes, err := s.client.DescribeIndex(ctx, CollectionName, vectorField)
	if err != nil && !isIndexNotFound(err) {
		return err
	}
	needCreate := err != nil
	for _, index := range indexes {
		if index.Name() == indexName {
			needCreate = false
			continue
		}
		// 已加载的 collection 不允许删索引，先释放再删除。
		if err := s.client.ReleaseCollection(ctx, CollectionName); err != nil {
			return err
		}
		if err := s.client.DropIndex(ctx, CollectionName, vectorField, client.WithIndexName(index.Name())); err != nil {
			return err
		}
		needCreate = true
		log.Printf("dropped milvus index %s (replaced by %s)", index.Name(), indexName)
	}
	if needCreate {
		index, err := entity.NewIndexHNSW(entity.COSINE, 16, 200)
		if err != nil {
			return err
		}
		if err := s.client.CreateIndex(ctx, CollectionName, vectorField, index, false, client.WithIndexName(indexName)); err != nil {
			return err
		}
		log.Printf("created milvus vector index %s", indexName)
	}
	return s.client.LoadCollection(ctx, CollectionName, false)
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
		state, err := s.client.GetIndexState(ctx, CollectionName, vectorField, client.WithIndexName(indexName))
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
	_, err := s.client.Upsert(ctx, CollectionName, "",
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
	param, err := entity.NewIndexHNSWSearchParam(64)
	if err != nil {
		return nil, err
	}
	results, err := s.client.Search(ctx, CollectionName, []string{}, "",
		[]string{primaryField},
		[]entity.Vector{entity.FloatVector(toFloat32(query))},
		vectorField, entity.COSINE, topK, param,
	)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return []VectorHit{}, nil
	}
	result := results[0]
	hits := make([]VectorHit, 0, result.ResultCount)
	for _, field := range result.Fields {
		idColumn, ok := field.(*entity.ColumnInt64)
		if !ok || field.Name() != primaryField {
			continue
		}
		for i := 0; i < result.ResultCount; i++ {
			hits = append(hits, VectorHit{
				VideoID:  idColumn.Data()[i],
				Distance: float64(result.Scores[i]),
			})
		}
	}
	return hits, nil
}

// EnsureReadyAsync 后台阻塞式重试初始化，直至成功或 ctx 取消；成功后回调 onReady。
func (s *MilvusStore) EnsureReadyAsync(ctx context.Context, retryInterval time.Duration, onReady func()) {
	if s == nil {
		return
	}
	for {
		err := s.EnsureReady(ctx)
		if err == nil {
			log.Printf("milvus collection %s ready", CollectionName)
			if onReady != nil {
				onReady()
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("milvus ensure ready failed: %v, retrying in %s", err, retryInterval)
		select {
		case <-time.After(retryInterval):
		case <-ctx.Done():
			return
		}
	}
}

// LazyStore 延迟初始化包装：后台重试连接与建集合，就绪前所有操作返回 ErrVectorStoreNotReady，
// 调用方（向量召回路/worker 双写）按降级策略处理，不阻塞服务启动。
type LazyStore struct {
	mu    sync.RWMutex
	store *MilvusStore
	ready []func()
}

func NewLazyStore(ctx context.Context, address string, retryInterval time.Duration) *LazyStore {
	ls := &LazyStore{}
	go func() {
		store := StartMilvusStore(ctx, address, retryInterval)
		if store == nil {
			return
		}
		store.EnsureReadyAsync(ctx, retryInterval, func() {
			ls.mu.Lock()
			ls.store = store
			callbacks := append([]func(){}, ls.ready...)
			ls.ready = nil
			ls.mu.Unlock()
			for _, cb := range callbacks {
				cb()
			}
		})
	}()
	return ls
}

// OnReady 注册就绪回调；若已就绪则立即异步执行。
func (s *LazyStore) OnReady(cb func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		go cb()
		return
	}
	s.ready = append(s.ready, cb)
}

func (s *LazyStore) getStore() *MilvusStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store
}

func (s *LazyStore) EnsureReady(ctx context.Context) error {
	if store := s.getStore(); store != nil {
		return store.EnsureReady(ctx)
	}
	return ErrVectorStoreNotReady
}

func (s *LazyStore) UpsertVideos(ctx context.Context, vectors []VideoVector) error {
	if store := s.getStore(); store != nil {
		return store.UpsertVideos(ctx, vectors)
	}
	return ErrVectorStoreNotReady
}

func (s *LazyStore) Search(ctx context.Context, query []float64, topK int) ([]VectorHit, error) {
	if store := s.getStore(); store != nil {
		return store.Search(ctx, query, topK)
	}
	return nil, ErrVectorStoreNotReady
}

func (s *LazyStore) Close() {
	if store := s.getStore(); store != nil {
		store.Close()
	}
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
