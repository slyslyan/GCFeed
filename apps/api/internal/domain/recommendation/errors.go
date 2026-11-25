package domainrecommendation

import (
	"errors"
	"fmt"
)

var ErrInvalidUserID = errors.New("invalid user id")
var ErrInvalidVideoID = errors.New("invalid video id")
var ErrInvalidLimit = errors.New("invalid limit")
var ErrEmptyScene = errors.New("scene is required")
var ErrSceneTooLong = errors.New("scene is too long")
var ErrRequestIDTooLong = errors.New("request id is too long")
var ErrInvalidCursor = errors.New("invalid cursor")
var ErrVideoNotFound = errors.New("video not found")

var ErrModelUnavailable = errors.New("recommendation model is unavailable")

// ErrSnapshotMissing allows a fresh feed request to use the timeline.
var ErrSnapshotMissing = fmt.Errorf("%w: snapshot missing", ErrModelUnavailable)
