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
	blocks     repository.AirworthinessBlockRepository
	security   SecurityService
}

func NewInspectionTaskService(repo repository.InspectionTaskRepository, blocks repository.AirworthinessBlockRepository, security SecurityService) InspectionTaskService {
	return &inspectionTaskService{repository: repo, blocks: blocks, security: security}
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
	_ = s.security.Audit(ctx, actor, requestID, "create", "InspectionTask", item.ID, "", item.Status, "created 检查任务")
	return item, nil
}

func (s *inspectionTaskService) Update(ctx context.Context, id uint, input dto.UpdateInspectionTask, actor, requestID string) (model.InspectionTask, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.InspectionTask{}, err
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
	_ = s.security.Audit(ctx, actor, requestID, "update", "InspectionTask", id, current.Status, current.Status, "updated business fields")
	return s.repository.Get(ctx, id)
}

func (s *inspectionTaskService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, requestID string) (model.InspectionTask, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.InspectionTask{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.InspectionTaskTransitions, current.Status, target) {
		return model.InspectionTask{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	reason := strings.TrimSpace(input.Reason)
	before := current.Status
	now := time.Now().UTC()
	change := repository.BlockChange{
		Actor: actor, RequestID: requestID, TaskCode: current.Code, TaskID: id,
		Reason: reason, OccurredAt: now,
	}

	switch {
	case before == "running" && target == "failed":
		// Failed judgment plus the same-code part/authorization cascade commit
		// together, including the task's own audit row, so a failure never leaves
		// a half update. The repository performs the version CAS against the
		// persisted "running" state; mutating current here would break it.
		if err := s.blocks.ApplyFailure(ctx, &current, input.ExpectedVersion, change); err != nil {
			return model.InspectionTask{}, fmt.Errorf("apply inspection failure block: %w", err)
		}
	case before == "failed" && target == "running":
		// Reopening for re-inspection keeps the failure reason and the block in
		// force; only a subsequent pass clears both.
		current.Status = target
		current.Version = input.ExpectedVersion + 1
		current.UpdatedAt = now
		if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
			return model.InspectionTask{}, fmt.Errorf("reopen 检查任务: %w", err)
		}
		if err := s.security.Audit(ctx, actor, requestID, "transition", "InspectionTask", id, before, target, reason); err != nil {
			return model.InspectionTask{}, fmt.Errorf("persist transition audit: %w", err)
		}
	case before == "running" && target == "passed":
		if strings.TrimSpace(current.FailureReason) != "" {
			// Re-inspection of a previously failed task. ClearFailure commits the
			// pass, the task audit row and the one-time recovery of stamped rows
			// atomically; when another same-code task is still failed the block
			// stays in force.
			otherFailure, err := s.blocks.ClearFailure(ctx, &current, input.ExpectedVersion, change)
			if err != nil {
				return model.InspectionTask{}, fmt.Errorf("release inspection failure block: %w", err)
			}
			if otherFailure {
				_ = s.security.Audit(ctx, actor, requestID, "airworthiness_block", "InspectionTask", id, before, target, "re-inspection passed but another failed task keeps the component blocked")
			}
		} else {
			current.Status = target
			current.Version = input.ExpectedVersion + 1
			current.UpdatedAt = now
			if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
				return model.InspectionTask{}, fmt.Errorf("transition 检查任务: %w", err)
			}
			if err := s.security.Audit(ctx, actor, requestID, "transition", "InspectionTask", id, before, target, reason); err != nil {
				return model.InspectionTask{}, fmt.Errorf("persist transition audit: %w", err)
			}
		}
	default:
		// planned -> running is the only remaining graph edge.
		current.Status = target
		current.Version = input.ExpectedVersion + 1
		current.UpdatedAt = now
		if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
			return model.InspectionTask{}, fmt.Errorf("transition 检查任务: %w", err)
		}
		if err := s.security.Audit(ctx, actor, requestID, "transition", "InspectionTask", id, before, target, reason); err != nil {
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
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	return s.security.Audit(ctx, actor, requestID, "delete", "InspectionTask", id, current.Status, "deleted", "soft deleted 检查任务")
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
