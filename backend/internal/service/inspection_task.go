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

type InspectionTaskService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.InspectionTask], error)
	Get(context.Context, uint) (model.InspectionTask, error)
	Create(context.Context, dto.CreateInspectionTask, string, string) (model.InspectionTask, error)
	Update(context.Context, uint, dto.UpdateInspectionTask, string, string) (model.InspectionTask, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string) (model.InspectionTask, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type inspectionTaskService struct {
	repository repository.InspectionTaskRepository
	blocking   repository.AirworthinessBlockRepository
	security   SecurityService
}

func NewInspectionTaskService(repo repository.InspectionTaskRepository, blocking repository.AirworthinessBlockRepository, security SecurityService) InspectionTaskService {
	return &inspectionTaskService{repository: repo, blocking: blocking, security: security}
}

func (s *inspectionTaskService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.InspectionTask], error) {
	return s.repository.List(ctx, query)
}

func (s *inspectionTaskService) Get(ctx context.Context, id uint) (model.InspectionTask, error) {
	return s.repository.Get(ctx, id)
}

func (s *inspectionTaskService) Create(ctx context.Context, input dto.CreateInspectionTask, actor, requestID string) (model.InspectionTask, error) {
	if err := validateInspectionTaskBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.InspectionTask{}, err
	}
	item := model.InspectionTask{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.InspectionTaskInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode: strings.ToUpper(strings.TrimSpace(input.RelatedCode)),
	}
	if err := s.repository.Create(ctx, &item); err != nil {
		return model.InspectionTask{}, fmt.Errorf("create 检查任务: %w", err)
	}
	_ = s.audit(ctx, actor, requestID, "create", "InspectionTask", item.ID, "", item.Status, "created 检查任务")
	return item, nil
}

func (s *inspectionTaskService) Update(ctx context.Context, id uint, input dto.UpdateInspectionTask, actor, requestID string) (model.InspectionTask, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.InspectionTask{}, err
	}
	// The failure origin links the blocking chain by RelatedCode; changing it
	// after a failure would detach blocked parts and authorizations.
	if current.Status == model.InspectionStatusFailed &&
		strings.ToUpper(strings.TrimSpace(input.RelatedCode)) != strings.ToUpper(strings.TrimSpace(current.RelatedCode)) {
		return model.InspectionTask{}, ErrAirworthinessHold
	}
	if err := validateInspectionTaskBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.InspectionTask{}, err
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
	if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
		return model.InspectionTask{}, fmt.Errorf("update 检查任务: %w", err)
	}
	_ = s.audit(ctx, actor, requestID, "update", "InspectionTask", id, current.Status, current.Status, "updated business fields")
	return s.repository.Get(ctx, id)
}

func (s *inspectionTaskService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, requestID string) (model.InspectionTask, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.InspectionTask{}, err
	}
	target := strings.TrimSpace(input.Status)
	reason := strings.TrimSpace(input.Reason)
	switch target {
	case model.InspectionStatusFailed, model.InspectionStatusPassed:
		// failed/passed can only be decided from running. A decision arriving
		// after the task has left running is a duplicate or concurrent
		// judgment: report a version conflict so callers refresh, and the
		// CAS below guarantees the cascade applies exactly once.
		if current.Status != model.InspectionStatusRunning {
			return model.InspectionTask{}, fmt.Errorf("%w: %s decision repeated on %s", repository.ErrVersionConflict, target, current.Status)
		}
		if target == model.InspectionStatusFailed {
			// Task decision, part pause, authorization restriction, revisions
			// and audits all share one transaction: no half-applied failure.
			if err := s.blocking.ApplyFailureBlock(ctx, current, input.ExpectedVersion, reason, actor, requestID); err != nil {
				return model.InspectionTask{}, fmt.Errorf("apply failure block: %w", err)
			}
		} else {
			// Passing a re-inspection clears the one-shot block;
			// authorizations must still be resubmitted for review afterwards.
			if err := s.blocking.ResolveFailureBlock(ctx, current, input.ExpectedVersion, actor, requestID); err != nil {
				return model.InspectionTask{}, fmt.Errorf("resolve failure block: %w", err)
			}
		}
	default:
		if !constants.CanTransition(constants.InspectionTaskTransitions, current.Status, target) {
			return model.InspectionTask{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
		}
		before := current.Status
		current.Status = target
		current.Version = input.ExpectedVersion + 1
		current.UpdatedAt = time.Now().UTC()
		if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
			return model.InspectionTask{}, fmt.Errorf("transition 检查任务: %w", err)
		}
		if err := s.audit(ctx, actor, requestID, "transition", "InspectionTask", id, before, target, reason); err != nil {
			return model.InspectionTask{}, fmt.Errorf("persist transition audit: %w", err)
		}
	}
	return s.repository.Get(ctx, id)
}

func (s *inspectionTaskService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status == model.InspectionStatusFailed {
		return ErrAirworthinessHold
	}
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	return s.audit(ctx, actor, requestID, "delete", "InspectionTask", id, current.Status, "deleted", "soft deleted 检查任务")
}

// audit tolerates a missing SecurityService so unit tests can exercise the
// state machine without wiring the security repository.
func (s *inspectionTaskService) audit(ctx context.Context, actor, requestID, action, entityType string, entityID uint, before, after, detail string) error {
	if s.security == nil {
		return nil
	}
	return s.security.Audit(ctx, actor, requestID, action, entityType, entityID, before, after, detail)
}

func (s *inspectionTaskService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func validateInspectionTaskBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}
