package infratwotower

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	recommendation "GCFeed/internal/domain/recommendation"
)

type snapshotCatalog struct{}

func (snapshotCatalog) PublishedCandidates(ctx context.Context, ids []int64) (map[int64]*recommendation.Candidate, error) {
	result := make(map[int64]*recommendation.Candidate, len(ids))
	for _, id := range ids {
		result[id] = &recommendation.Candidate{VideoID: id, AuthorID: id, PublishedAt: time.Unix(1, 0), Source: recommendation.SourceVector}
	}
	return result, nil
}

// The trainer and API live in different languages. Exercise their artifact contract directly.
func TestServeTrainedSnapshot(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for training smoke test")
	}
	file := filepath.Join(t.TempDir(), "model.json")
	cmd := exec.Command(py, "../../../../../tools/twotower/train.py", "--demo", "--epochs", "1", "--output", file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("train demo: %v: %s", err, out)
	}
	r, err := Open(context.Background(), file, "", snapshotCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	var knownID int64
	for userID := range r.snapshot.Users {
		knownID = userID
		break
	}
	if knownID == 0 {
		t.Fatal("trainer did not export any known users")
	}
	for userID, reason := range map[int64]string{knownID: "interest_match", 9223372036854775807: "cold_start"} {
		hits, err := r.Recall(context.Background(), userID, 5)
		if err != nil || len(hits) == 0 {
			t.Fatalf("user %d: empty retrieval, %v", userID, err)
		}
		for _, hit := range hits {
			if hit.Reason != reason || hit.ModelVersion == "" || r.snapshot.Videos[hit.VideoID] == nil {
				t.Fatalf("unexpected hit %+v", hit)
			}
		}
	}
}

func TestRejectMismatchedVectors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "model.json")
	s := snapshot{Dimension: 128, Version: "invalid", DefaultUser: make([]float64, 128), Videos: map[int64][]float64{1: {1, 0}}}
	s.DefaultUser[0] = 1
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), file, "", snapshotCatalog{}); err == nil {
		t.Fatal("accepted snapshot with incompatible video vectors")
	}
}
