package domainrecommendation

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestCursorValidModelAndCosineScore(t *testing.T) {
	for _, score := range []float64{-1, -.25, 0, .8, 1} {
		cursor := &Cursor{ModelVersion: "two-tower-v1", RankScore: score, PublishedAt: time.Now(), VideoID: 1}
		if !cursor.Valid() {
			t.Fatalf("valid cosine score rejected: %f", score)
		}
	}
	for _, score := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1.01, 1.01} {
		cursor := &Cursor{ModelVersion: "two-tower-v1", RankScore: score, PublishedAt: time.Now(), VideoID: 1}
		if cursor.Valid() {
			t.Fatalf("invalid cosine score accepted: %f", score)
		}
	}
	for _, version := range []string{"", "  ", strings.Repeat("x", MaxModelVersionLength+1)} {
		cursor := &Cursor{ModelVersion: version, RankScore: .5, PublishedAt: time.Now(), VideoID: 1}
		if cursor.Valid() {
			t.Fatalf("invalid model version accepted: %q", version)
		}
	}
}
