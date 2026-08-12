package applicationembedding

import (
	applicationvideo "GCFeed/internal/application/video"
	domainembedding "GCFeed/internal/domain/embedding"
	infravector "GCFeed/internal/infra/vector"
	"context"
	"encoding/json"
	"errors"
	"log"
)

var ErrSaveVideoEmbeddingFailed = errors.New("failed to save video embedding")
var ErrMarshalEmbeddingFailed = errors.New("failed to marshal embedding")

type Service struct {
	repo        domainembedding.Repository
	vectorizer  domainembedding.Vectorizer
	vectorStore infravector.VectorStore
}

type Option func(*Service)

// WithVectorStore 注入向量库（Milvus），向量写入失败只记日志，不阻塞 embedding 落库链路。
func WithVectorStore(store infravector.VectorStore) Option {
	return func(s *Service) { s.vectorStore = store }
}

type GenerateVideoEmbeddingResult struct {
	Embedding        *domainembedding.VideoEmbedding
	CreatedOrUpdated bool
}

func New(repo domainembedding.Repository, vectorizer domainembedding.Vectorizer, opts ...Option) *Service {
	if vectorizer == nil {
		vectorizer = domainembedding.NewHashNgramVectorizer()
	}
	s := &Service{
		repo:       repo,
		vectorizer: vectorizer,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// GenerateForPublishedVideo 根据视频发布事件生成并保存视频内容向量。
func (s *Service) GenerateForPublishedVideo(ctx context.Context, event *applicationvideo.PublishedEvent) (*GenerateVideoEmbeddingResult, error) {
	if event == nil || event.VideoID <= 0 {
		return &GenerateVideoEmbeddingResult{}, nil
	}

	text := domainembedding.BuildVideoText(event.Title, event.Description)
	vector := s.vectorizer.Vectorize(text)
	content, err := json.Marshal(vector)
	if err != nil {
		return nil, ErrMarshalEmbeddingFailed
	}

	embedding := domainembedding.NewVideoEmbedding(
		event.VideoID,
		s.vectorizer.Model(),
		vector,
		domainembedding.TextHash(text),
		string(content),
	)
	if err := s.repo.SaveVideoEmbedding(ctx, embedding); err != nil {
		return nil, ErrSaveVideoEmbeddingFailed
	}
	s.syncToVectorStore(ctx, event.VideoID, vector)

	return &GenerateVideoEmbeddingResult{
		Embedding:        embedding,
		CreatedOrUpdated: true,
	}, nil
}

// BackfillVectorStore 将 MySQL 存量 embedding 分批 upsert 到向量库（按主键幂等），
// 维度与集合不符的行跳过；供 worker 启动时对既有视频补一次向量库回填。
func (s *Service) BackfillVectorStore(ctx context.Context, batchSize int) error {
	if s == nil || s.vectorStore == nil {
		return nil
	}
	const pageSize = 500
	if batchSize <= 0 {
		batchSize = pageSize
	}
	var afterID int64
	upserted := 0
	for {
		embeddings, err := s.repo.ListAllVideoEmbeddings(ctx, afterID, pageSize)
		if err != nil {
			return err
		}
		if len(embeddings) == 0 {
			break
		}
		var batch []infravector.VideoVector
		for _, embedding := range embeddings {
			// 游标只增不减：坏行跳过也要推进，否则末页不满一批时同一批行会反复返回。
			afterID = embedding.VideoID
			vec, err := parseEmbeddingVector(embedding)
			if err != nil {
				log.Printf("backfill skip video %d: %v", embedding.VideoID, err)
				continue
			}
			// 以解析后的实际长度为准（Dimension 列可能与 JSON 内容不一致），保证整批向量维度统一。
			if len(vec) != infravector.VectorDimension {
				log.Printf("backfill skip video %d: vector length %d != %d", embedding.VideoID, len(vec), infravector.VectorDimension)
				continue
			}
			batch = append(batch, infravector.VideoVector{VideoID: embedding.VideoID, Vector: vec})
			if len(batch) >= batchSize {
				if err := s.upsertVectorStore(ctx, batch); err != nil {
					return err
				}
				upserted += len(batch)
				batch = batch[:0]
			}
		}
		if len(batch) > 0 {
			if err := s.upsertVectorStore(ctx, batch); err != nil {
				return err
			}
			upserted += len(batch)
		}
	}
	log.Printf("milvus backfill done, upserted %d videos", upserted)
	return nil
}

// syncToVectorStore 双写向量库；失败只记日志，消息流不因 Milvus 故障重试卡死。
func (s *Service) syncToVectorStore(ctx context.Context, videoID int64, vector []float64) {
	if s.vectorStore == nil {
		return
	}
	if err := s.vectorStore.UpsertVideos(ctx, []infravector.VideoVector{{VideoID: videoID, Vector: vector}}); err != nil {
		log.Printf("upsert video %d vector to milvus failed: %v", videoID, err)
	}
}

func (s *Service) upsertVectorStore(ctx context.Context, batch []infravector.VideoVector) error {
	if err := s.vectorStore.UpsertVideos(ctx, batch); err != nil {
		log.Printf("upsert %d vectors to milvus failed: %v", len(batch), err)
		return err
	}
	return nil
}

func parseEmbeddingVector(embedding *domainembedding.VideoEmbedding) ([]float64, error) {
	if len(embedding.Embedding) > 0 {
		return embedding.Embedding, nil
	}
	vec := make([]float64, 0, embedding.Dimension)
	if err := json.Unmarshal([]byte(embedding.EmbeddingJSON), &vec); err != nil {
		return nil, err
	}
	return vec, nil
}
