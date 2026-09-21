package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/constants"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
)

type ReleaseAuthorizationService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.ReleaseAuthorization], error)
	Get(context.Context, uint) (model.ReleaseAuthorization, error)
	Create(context.Context, dto.CreateReleaseAuthorization, string, string) (model.ReleaseAuthorization, error)
	Update(context.Context, uint, dto.UpdateReleaseAuthorization, string, string) (model.ReleaseAuthorization, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string, string) (model.ReleaseAuthorization, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type releaseAuthorizationService struct {
	repository repository.ReleaseAuthorizationRepository
	blocker    AirworthinessBlocker
	security   SecurityService
}

func NewReleaseAuthorizationService(repo repository.ReleaseAuthorizationRepository, blocker AirworthinessBlocker, security SecurityService) ReleaseAuthorizationService {
	return &releaseAuthorizationService{repository: repo, blocker: blocker, security: security}
}

func (s *releaseAuthorizationService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.ReleaseAuthorization], error) {
	return s.repository.List(ctx, query)
}

func (s *releaseAuthorizationService) Get(ctx context.Context, id uint) (model.ReleaseAuthorization, error) {
	return s.repository.Get(ctx, id)
}

func (s *releaseAuthorizationService) Create(ctx context.Context, input dto.CreateReleaseAuthorization, actor, requestID string) (model.ReleaseAuthorization, error) {
	if err := validateReleaseAuthorizationBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ReleaseAuthorization{}, err
	}
	item := model.ReleaseAuthorization{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.ReleaseAuthorizationInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode: strings.ToUpper(strings.TrimSpace(input.RelatedCode)),
	}
	if err := s.repository.CreateVersion(ctx, &item, actor, requestID); err != nil {
		return model.ReleaseAuthorization{}, fmt.Errorf("create 放行授权: %w", err)
	}
	return s.repository.Get(ctx, item.ID)
}

func (s *releaseAuthorizationService) Update(ctx context.Context, id uint, input dto.UpdateReleaseAuthorization, actor, requestID string) (model.ReleaseAuthorization, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ReleaseAuthorization{}, err
	}
	if current.Status != model.ReleaseAuthorizationInitialStatus {
		return model.ReleaseAuthorization{}, ErrLocked
	}
	if err := validateReleaseAuthorizationBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ReleaseAuthorization{}, err
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = strings.TrimSpace(input.Facility)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateVersion(ctx, id, input.ExpectedVersion, &current, actor, requestID, "update", current.Status, "draft authorization fields updated"); err != nil {
		return model.ReleaseAuthorization{}, fmt.Errorf("update 放行授权: %w", err)
	}
	return s.repository.Get(ctx, id)
}

func (s *releaseAuthorizationService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, role, requestID string) (model.ReleaseAuthorization, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ReleaseAuthorization{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.ReleaseAuthorizationTransitions, current.Status, target) {
		return model.ReleaseAuthorization{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	if !isOperatorRole(role) {
		return model.ReleaseAuthorization{}, ErrForbidden
	}
	if current.Status == "restricted" && target == "review" {
		// The only way out of restricted back to review is a released inspection
		// block: a reviewer-imposed restriction must follow the normal restricted
		// lifecycle, and re-submission is forbidden while the failure is active.
		// The resulting review must still pass dual-control approval afterwards.
		// Clearing the stamp closes this blocking episode so a later reviewer
		// restriction on the same authorization cannot re-use this gateway.
		if strings.TrimSpace(current.BlockingTaskCode) == "" {
			return model.ReleaseAuthorization{}, fmt.Errorf("%w: restriction was imposed by a reviewer, not by an inspection failure", ErrInvalidTransition)
		}
		if err := s.blocker.EnsureClear(ctx, current.RelatedCode); err != nil {
			return model.ReleaseAuthorization{}, err
		}
		current.SubmittedBy = actor
		current.ReviewedBy = ""
		current.ReviewReason = ""
		current.BlockingTaskCode = ""
		current.BlockingReason = ""
	} else if target == "review" {
		current.SubmittedBy = actor
		current.ReviewedBy = ""
		current.ReviewReason = ""
	}
	if target == "approved" {
		// A blocked component can never be approved, regardless of the row state.
		active, taskCode, _, err := s.blocker.ActiveFailure(ctx, current.RelatedCode)
		if err != nil {
			return model.ReleaseAuthorization{}, fmt.Errorf("check airworthiness block: %w", err)
		}
		if active {
			return model.ReleaseAuthorization{}, fmt.Errorf("%w: 检查任务 %s 失败待复核", ErrAirworthinessHold, taskCode)
		}
	}
	if target == "approved" || target == "restricted" || target == "revoked" || target == "draft" {
		if !isReviewerRole(role) {
			return model.ReleaseAuthorization{}, ErrForbidden
		}
		if current.SubmittedBy != "" && actor == current.SubmittedBy {
			return model.ReleaseAuthorization{}, ErrSeparationOfDuty
		}
		current.ReviewedBy = actor
		current.ReviewReason = strings.TrimSpace(input.Reason)
	}
	before := current.Status
	current.Status = target
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.UpdateVersion(ctx, id, input.ExpectedVersion, &current, actor, requestID, "transition", before, strings.TrimSpace(input.Reason)); err != nil {
		return model.ReleaseAuthorization{}, fmt.Errorf("transition 放行授权: %w", err)
	}
	return s.repository.Get(ctx, id)
}

func (s *releaseAuthorizationService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status != model.ReleaseAuthorizationInitialStatus {
		return ErrLocked
	}
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	return s.security.Audit(ctx, actor, requestID, "delete", "ReleaseAuthorization", id, current.Status, "deleted", "soft deleted 放行授权")
}

func (s *releaseAuthorizationService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func validateReleaseAuthorizationBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}

func isOperatorRole(role string) bool {
	return role == model.RoleOperator || role == model.RoleReviewer || role == model.RoleAdmin
}

func isReviewerRole(role string) bool {
	return role == model.RoleReviewer || role == model.RoleAdmin
}
