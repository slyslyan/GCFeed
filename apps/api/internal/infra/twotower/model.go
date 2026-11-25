// Package infratwotower serves immutable user and video vectors from the same training run.
package infratwotower

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	recommendation "GCFeed/internal/domain/recommendation"
	vector "GCFeed/internal/infra/vector"
)

type snapshot struct {
	Version     string              `json:"version"`
	Dimension   int                 `json:"dimension"`
	DefaultUser []float64           `json:"default_user"`
	Users       map[int64][]float64 `json:"users"`
	Videos      map[int64][]float64 `json:"videos"`
}

type Catalog interface {
	PublishedCandidates(context.Context, []int64) (map[int64]*recommendation.Candidate, error)
}

type Recaller struct {
	snapshot *snapshot
	digest   [sha256.Size]byte
	catalog  Catalog
	remote   bool
	store    atomic.Pointer[vector.MilvusStore]
}

func validateVector(v []float64) bool {
	if len(v) != vector.VectorDimension {
		return false
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	return math.Abs(norm-1) < 0.001
}

func load(path string) (*snapshot, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return nil, digest, err
	}
	defer f.Close()
	hash := sha256.New()
	dec := json.NewDecoder(io.TeeReader(f, hash))
	var s snapshot
	if err := dec.Decode(&s); err != nil {
		return nil, digest, err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, digest, errors.New("trailing data in model snapshot")
	}
	if strings.TrimSpace(s.Version) == "" || s.Dimension != vector.VectorDimension || !validateVector(s.DefaultUser) || len(s.Videos) == 0 {
		return nil, digest, errors.New("invalid snapshot version, dimension, default user or catalog")
	}
	for _, vectors := range []map[int64][]float64{s.Users, s.Videos} {
		for id, v := range vectors {
			if id <= 0 || !validateVector(v) {
				return nil, digest, fmt.Errorf("invalid vector for id %d", id)
			}
		}
	}
	copy(digest[:], hash.Sum(nil))
	return &s, digest, nil
}

// Open pins the artifact for the API process. Replacing the file takes effect on restart.
func Open(ctx context.Context, path, address string, catalog Catalog) (*Recaller, error) {
	r := &Recaller{catalog: catalog, remote: address != ""}
	s, digest, err := load(path)
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("two-tower snapshot missing: %s", path)
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read two-tower snapshot: %w", err)
	}
	r.snapshot, r.digest = s, digest
	if r.remote {
		go r.prepareIndex(ctx, address)
	}
	return r, nil
}

// Each process becomes ready only after importing its complete snapshot. Retries are idempotent.
func (r *Recaller) prepareIndex(ctx context.Context, address string) {
	for ctx.Err() == nil {
		store, err := vector.NewSnapshotStore(ctx, address, r.digest)
		if err == nil {
			err = r.importVideos(ctx, store)
			if err == nil {
				r.store.Store(store)
				log.Printf("two-tower index ready: version=%s videos=%d", r.snapshot.Version, len(r.snapshot.Videos))
				if ctx.Done() != nil {
					<-ctx.Done()
					store.Close()
				}
				return
			}
			store.Close()
		}
		log.Printf("two-tower index preparation failed: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (r *Recaller) importVideos(ctx context.Context, store vector.VectorStore) error {
	if err := store.EnsureReady(ctx); err != nil {
		return err
	}
	batch := make([]vector.VideoVector, 0, 500)
	for id, v := range r.snapshot.Videos {
		batch = append(batch, vector.VideoVector{VideoID: id, Vector: v})
		if len(batch) == cap(batch) {
			if err := store.UpsertVideos(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		return store.UpsertVideos(ctx, batch)
	}
	return nil
}

func (r *Recaller) Recall(ctx context.Context, userID int64, limit int) ([]*recommendation.Candidate, error) {
	if r.snapshot == nil {
		return nil, recommendation.ErrSnapshotMissing
	}
	query, ok := r.snapshot.Users[userID]
	reason := "interest_match"
	if !ok {
		query = r.snapshot.DefaultUser
		reason = "cold_start"
	}
	n := min(limit*3, len(r.snapshot.Videos))
	if n <= 0 {
		return nil, nil
	}
	var hits []vector.VectorHit
	if r.remote {
		store := r.store.Load()
		if store == nil {
			return nil, recommendation.ErrModelUnavailable
		}
		var err error
		hits, err = store.Search(ctx, query, n)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", recommendation.ErrModelUnavailable, err)
		}
	} else {
		// Small local catalogs can use exact cosine search without running Milvus.
		for id, v := range r.snapshot.Videos {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var score float64
			for i, x := range v {
				score += query[i] * x
			}
			hits = append(hits, vector.VectorHit{VideoID: id, Distance: score})
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].Distance == hits[j].Distance {
				return hits[i].VideoID > hits[j].VideoID
			}
			return hits[i].Distance > hits[j].Distance
		})
		hits = hits[:n]
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.VideoID
	}
	cards, err := r.catalog.PublishedCandidates(ctx, ids)
	if err != nil {
		return nil, err
	}
	version := hex.EncodeToString(r.digest[:])
	candidates := make([]*recommendation.Candidate, 0, len(hits))
	for _, hit := range hits {
		if c := cards[hit.VideoID]; c != nil {
			c.Similarity = max(-1, min(1, hit.Distance))
			c.ModelVersion = version
			c.Reason = reason
			candidates = append(candidates, c)
		}
	}
	return candidates, nil
}
