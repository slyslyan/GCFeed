package domainrecommendation

import (
	"context"
	"time"
)

type Repository interface {
	ListRecentExposures(ctx context.Context, userID int64, videoIDs []int64, since time.Time) ([]*Exposure, error)
	SaveExposures(ctx context.Context, exposures []*ExposureWrite) ([]*Exposure, error)
}
