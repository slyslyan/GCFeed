package domainsession

import "errors"

var (
	ErrInvalidUserID             = errors.New("user id must be positive")
	ErrInvalidFamilyID           = errors.New("family id is required")
	ErrInvalidTokenHash          = errors.New("token hash is required")
	ErrInvalidRefreshTTL         = errors.New("refresh ttl must be positive")
	ErrRefreshTokenNotFound      = errors.New("refresh token not found")
	ErrRefreshTokenExpired       = errors.New("refresh token expired")
	ErrRefreshTokenReuseDetected = errors.New("refresh token reuse detected")
)
