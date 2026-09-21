package service

import (
	"context"
	"strings"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
)

// AirworthinessBlocker is the service-level guard used by the part and
// authorization services to keep failed-inspection components from being
// released or approved. It also drives the one-time failure/recovery cascade.
type AirworthinessBlocker interface {
	// ActiveFailure reports whether a failed inspection task still blocks the
	// given component code, together with the task code and failure reason.
	ActiveFailure(ctx context.Context, relatedCode string) (bool, string, string, error)
	// EnsureClear returns ErrBlockStillActive while an inspection failure for
	// the code has not passed re-inspection.
	EnsureClear(ctx context.Context, relatedCode string) error
}

type airworthinessBlockerService struct {
	repository repository.AirworthinessBlockRepository
}

func NewAirworthinessBlockerService(repo repository.AirworthinessBlockRepository) AirworthinessBlocker {
	return &airworthinessBlockerService{repository: repo}
}

func (s *airworthinessBlockerService) ActiveFailure(ctx context.Context, relatedCode string) (bool, string, string, error) {
	return s.repository.ActiveFailure(ctx, strings.TrimSpace(relatedCode))
}

func (s *airworthinessBlockerService) EnsureClear(ctx context.Context, relatedCode string) error {
	active, _, _, err := s.repository.ActiveFailure(ctx, relatedCode)
	if err != nil {
		return err
	}
	if active {
		return ErrBlockStillActive
	}
	return nil
}
