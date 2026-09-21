package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrNoActiveBlock signals that a blocking/recovery update matched no rows.
var ErrNoActiveBlock = errors.New("airworthiness block state changed concurrently")

// AirworthinessBlockRepository owns the cross-aggregate transaction that links
// inspection failures to 同编号部件 (same RelatedCode) parts and release
// authorizations. Every cascade (task decision + part pause + authorization
// restriction + versions + audits) commits in a single database transaction so
// a failure judgment can never leave a half-applied block.
type AirworthinessBlockRepository interface {
	// ApplyFailureBlock moves the inspection task to failed and, in the same
	// transaction, pauses same-code parts and forces review/approved
	// authorizations to restricted.
	ApplyFailureBlock(ctx context.Context, task model.InspectionTask, expectedVersion uint, reason, actor, requestID string) error
	// ResolveFailureBlock marks the re-inspection passed and clears the
	// one-shot block on same-code parts/authorizations only when no other
	// failed inspection still covers the code.
	ResolveFailureBlock(ctx context.Context, task model.InspectionTask, expectedVersion uint, actor, requestID string) error
	// HasFailedInspection reports whether an unresolved failed inspection
	// exists for the related code.
	HasFailedInspection(ctx context.Context, relatedCode string) (bool, error)
}

type airworthinessBlockRepository struct {
	db *gorm.DB
}

func NewAirworthinessBlockRepository(db *gorm.DB) AirworthinessBlockRepository {
	return &airworthinessBlockRepository{db: db}
}

// blockingAudit stores the audit payload written inside the cascade
// transaction before the blocking origin is committed.
type blockingAudit struct {
	entityType string
	entityID   uint
	before     string
	after      string
	action     string
	detail     string
}

func (r *airworthinessBlockRepository) ApplyFailureBlock(ctx context.Context, task model.InspectionTask, expectedVersion uint, reason, actor, requestID string) error {
	reason = strings.TrimSpace(reason)
	relatedCode := strings.ToUpper(strings.TrimSpace(task.RelatedCode))
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()

		// CAS the task decision first. A judgment is valid only from running
		// with the expected version. A concurrent or duplicate judgment loses
		// the row match (status has left running or the version moved) so the
		// whole cascade rolls back: the failure applies exactly once.
		result := tx.Model(&model.InspectionTask{}).
			Where("id = ? AND version = ? AND status = ?", task.ID, expectedVersion, model.InspectionStatusRunning).
			Updates(map[string]interface{}{
				"status":         model.InspectionStatusFailed,
				"failure_reason": reason,
				"version":        gorm.Expr("version + 1"),
				"updated_at":     now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrVersionConflict
		}

		audits := []blockingAudit{{
			entityType: "InspectionTask", entityID: task.ID, before: model.InspectionStatusRunning,
			after: model.InspectionStatusFailed, action: "transition", detail: reason,
		}}

		if relatedCode != "" {
			partAudits, err := pauseParts(tx, relatedCode, task.Code, reason, now)
			if err != nil {
				return err
			}
			authAudits, err := restrictAuthorizations(tx, relatedCode, task.Code, reason, actor, requestID, now)
			if err != nil {
				return err
			}
			audits = append(audits, partAudits...)
			audits = append(audits, authAudits...)
		}

		for _, entry := range audits {
			if err := appendAudit(tx, actor, requestID, entry.action, entry.entityType, entry.entityID, entry.before, entry.after, entry.detail); err != nil {
				return err
			}
		}
		return nil
	})
}

// pauseParts moves every same-code part that is not already on an active
// inspection block to hold. The conditional update makes repeated or
// concurrent judgments idempotent: a part blocked by another in-flight failure
// is never paused twice.
func pauseParts(tx *gorm.DB, relatedCode, taskCode, reason string, now time.Time) ([]blockingAudit, error) {
	var parts []model.AircraftPart
	if err := tx.Where("related_code = ? AND deleted_at IS NULL AND status <> ?", relatedCode, "retired").
		Find(&parts).Error; err != nil {
		return nil, err
	}
	audits := make([]blockingAudit, 0)
	for _, part := range parts {
		result := tx.Model(&model.AircraftPart{}).
			Where("id = ? AND (blocked_by_task_code = '' OR block_resolved_at IS NOT NULL)", part.ID).
			Updates(map[string]interface{}{
				"status":               "hold",
				"pre_block_status":     part.Status,
				"blocked_by_task_code": taskCode,
				"block_reason":         reason,
				"blocked_at":           now,
				"block_resolved_at":    nil,
				"version":              gorm.Expr("version + 1"),
				"updated_at":           now,
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		audits = append(audits, blockingAudit{
			entityType: "AircraftPart", entityID: part.ID, before: part.Status, after: "hold",
			action: "airworthiness-block",
			detail: fmt.Sprintf("检查任务 %s 判定失败，部件暂停: %s", taskCode, reason),
		})
	}
	return audits, nil
}

// restrictAuthorizations forces review/approved same-code authorizations to
// restricted and appends the immutable version revision. It also re-activates
// a previously failure-blocked authorization whose block was resolved but
// which has not been resubmitted yet (still restricted). Reviewer-decided
// restrictions and already-active blocks stay untouched, so repeated and
// concurrent judgments are idempotent.
func restrictAuthorizations(tx *gorm.DB, relatedCode, taskCode, reason, actor, requestID string, now time.Time) ([]blockingAudit, error) {
	var authorizations []model.ReleaseAuthorization
	if err := tx.Where("related_code = ? AND deleted_at IS NULL AND ("+
		"status IN ? OR (status = ? AND blocked_by_task_code <> ? AND block_resolved_at IS NOT NULL)"+
		")", relatedCode, []string{"review", "approved"}, "restricted", "").
		Find(&authorizations).Error; err != nil {
		return nil, err
	}
	audits := make([]blockingAudit, 0)
	for _, authorization := range authorizations {
		result := tx.Model(&model.ReleaseAuthorization{}).
			Where("id = ? AND deleted_at IS NULL AND ("+
				"status IN ? OR (status = ? AND blocked_by_task_code <> ? AND block_resolved_at IS NOT NULL)"+
				")", authorization.ID, []string{"review", "approved"}, "restricted", "").
			Updates(map[string]interface{}{
				"status":               "restricted",
				"blocked_by_task_code": taskCode,
				"block_reason":         reason,
				"blocked_at":           now,
				"block_resolved_at":    nil,
				"version":              gorm.Expr("version + 1"),
				"updated_at":           now,
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		// Re-read inside the transaction to obtain the actual post-update
		// version, so the revision cannot collide with a concurrently
		// committed revision on the same authorization.
		var updated model.ReleaseAuthorization
		if err := tx.First(&updated, authorization.ID).Error; err != nil {
			return nil, err
		}
		detail := fmt.Sprintf("检查任务 %s 判定失败，待复核/已批准授权转为受限: %s", taskCode, reason)
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: authorization.ID, Version: updated.Version, Status: "restricted",
			Evidence: updated.Evidence, Actor: actor, RequestID: requestID,
			Action: "airworthiness-block", Reason: detail, CreatedAt: now,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return nil, err
		}
		audits = append(audits, blockingAudit{
			entityType: "ReleaseAuthorization", entityID: authorization.ID,
			before: authorization.Status, after: "restricted", action: "airworthiness-block", detail: detail,
		})
	}
	return audits, nil
}

func (r *airworthinessBlockRepository) ResolveFailureBlock(ctx context.Context, task model.InspectionTask, expectedVersion uint, actor, requestID string) error {
	relatedCode := strings.ToUpper(strings.TrimSpace(task.RelatedCode))
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()

		// CAS the passed decision from running. Only one concurrent
		// re-inspection can win; the loser rolls back and the recovery side
		// effects stay single-shot.
		result := tx.Model(&model.InspectionTask{}).
			Where("id = ? AND version = ? AND status = ?", task.ID, expectedVersion, model.InspectionStatusRunning).
			Updates(map[string]interface{}{
				"status":         model.InspectionStatusPassed,
				"failure_reason": "",
				"version":        gorm.Expr("version + 1"),
				"updated_at":     now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrVersionConflict
		}

		audits := []blockingAudit{{
			entityType: "InspectionTask", entityID: task.ID, before: model.InspectionStatusRunning,
			after: model.InspectionStatusPassed, action: "transition", detail: "重新检查通过",
		}}

		if relatedCode != "" {
			remaining, err := countOpenFailures(tx, r.db.Dialector.Name(), relatedCode, task.ID)
			if err != nil {
				return err
			}
			// Another failed inspection still covers the code: keep the
			// block closed. The task transition itself still commits.
			if remaining == 0 {
				partAudits, err := resumeParts(tx, relatedCode, task.Code, now)
				if err != nil {
					return err
				}
				authAudits, err := unblockAuthorizations(tx, relatedCode, task.Code, actor, requestID, now)
				if err != nil {
					return err
				}
				audits = append(audits, partAudits...)
				audits = append(audits, authAudits...)
			}
		}

		for _, entry := range audits {
			if err := appendAudit(tx, actor, requestID, entry.action, entry.entityType, entry.entityID, entry.before, entry.after, entry.detail); err != nil {
				return err
			}
		}
		return nil
	})
}

// countOpenFailures counts failed inspections for the code while excluding the
// task just decided. On transactional engines a locking read is used so that a
// concurrent failure/pass decision for another same-code task is waited out
// before deciding whether the block can close; SQLite serializes writers
// itself and does not understand FOR UPDATE.
func countOpenFailures(tx *gorm.DB, dialect, relatedCode string, excludingTaskID uint) (int64, error) {
	query := tx.Model(&model.InspectionTask{}).
		Where("related_code = ? AND status = ? AND deleted_at IS NULL AND id <> ?",
			relatedCode, model.InspectionStatusFailed, excludingTaskID)
	if dialect != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var remaining int64
	return remaining, query.Count(&remaining).Error
}

// resumeParts restores parts paused by the block exactly once. The
// block_resolved_at IS NULL predicate makes a duplicate or concurrent
// resolution match zero rows.
func resumeParts(tx *gorm.DB, relatedCode, taskCode string, now time.Time) ([]blockingAudit, error) {
	var parts []model.AircraftPart
	if err := tx.Where("related_code = ? AND deleted_at IS NULL AND blocked_by_task_code <> ? AND block_resolved_at IS NULL",
		relatedCode, "").Find(&parts).Error; err != nil {
		return nil, err
	}
	audits := make([]blockingAudit, 0)
	for _, part := range parts {
		target := part.PreBlockStatus
		if target == "" || target == "retired" {
			target = "inspection"
		}
		updates := map[string]interface{}{
			"block_resolved_at": now,
			"version":           gorm.Expr("version + 1"),
			"updated_at":        now,
		}
		after := target
		// The service closes all manual moves during an active block, but stay
		// defensive: a part moved out of hold concurrently keeps its current
		// status while the block still resolves exactly once.
		if part.Status == "hold" {
			updates["status"] = target
		} else {
			after = part.Status
		}
		result := tx.Model(&model.AircraftPart{}).
			Where("id = ? AND block_resolved_at IS NULL", part.ID).
			Updates(updates)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		audits = append(audits, blockingAudit{
			entityType: "AircraftPart", entityID: part.ID, before: "hold", after: after,
			action: "airworthiness-unblock",
			detail: fmt.Sprintf("检查任务 %s 重新检查通过，解除部件暂停", taskCode),
		})
	}
	return audits, nil
}

// unblockAuthorizations clears the block on restricted authorizations but
// deliberately leaves their status at restricted: a passed re-inspection only
// permits a fresh restricted -> review resubmission into the existing
// two-person review, never an automatic approval.
func unblockAuthorizations(tx *gorm.DB, relatedCode, taskCode, actor, requestID string, now time.Time) ([]blockingAudit, error) {
	var candidates []uint
	if err := tx.Model(&model.ReleaseAuthorization{}).
		Where("related_code = ? AND deleted_at IS NULL AND status = ? AND blocked_by_task_code <> ? AND block_resolved_at IS NULL",
			relatedCode, "restricted", "").
		Pluck("id", &candidates).Error; err != nil {
		return nil, err
	}
	audits := make([]blockingAudit, 0)
	for _, id := range candidates {
		result := tx.Model(&model.ReleaseAuthorization{}).
			Where("id = ? AND status = ? AND block_resolved_at IS NULL", id, "restricted").
			Updates(map[string]interface{}{
				"block_resolved_at": now,
				"version":           gorm.Expr("version + 1"),
				"updated_at":        now,
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		var updated model.ReleaseAuthorization
		if err := tx.First(&updated, id).Error; err != nil {
			return nil, err
		}
		detail := fmt.Sprintf("检查任务 %s 重新检查通过，授权可重新提交复核", taskCode)
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: id, Version: updated.Version, Status: "restricted",
			Evidence: updated.Evidence, Actor: actor, RequestID: requestID,
			Action: "airworthiness-unblock", Reason: detail, CreatedAt: now,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return nil, err
		}
		audits = append(audits, blockingAudit{
			entityType: "ReleaseAuthorization", entityID: id,
			before: "restricted", after: "restricted", action: "airworthiness-unblock", detail: detail,
		})
	}
	return audits, nil
}

func (r *airworthinessBlockRepository) HasFailedInspection(ctx context.Context, relatedCode string) (bool, error) {
	relatedCode = strings.ToUpper(strings.TrimSpace(relatedCode))
	if relatedCode == "" {
		return false, nil
	}
	var count int64
	if err := r.db.WithContext(ctx).Model(&model.InspectionTask{}).
		Where("related_code = ? AND status = ? AND deleted_at IS NULL", relatedCode, model.InspectionStatusFailed).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
