package repository

import (
	"context"
	"strings"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BlockChange carries the actor/request attribution and the failed inspection
// identity stamped onto every blocked part and authorization.
type BlockChange struct {
	Actor      string
	RequestID  string
	TaskCode   string
	TaskID     uint
	Reason     string
	OccurredAt time.Time
}

// AirworthinessBlockRepository owns the cross-aggregate airworthiness blocking
// closure: a failed inspection task, the parts and release authorizations that
// share its RelatedCode, their revisions and audit rows must all move inside a
// single database transaction so a failure can never leave a half update.
type AirworthinessBlockRepository interface {
	// ApplyFailure marks the task failed and atomically puts same-code parts on
	// hold and review/approved authorizations under restricted release. Rows
	// that are already blocked are left untouched, which makes repeated and
	// concurrent failure judgments effective only once. Returns
	// ErrVersionConflict when the task moved concurrently.
	ApplyFailure(context.Context, *model.InspectionTask, uint, BlockChange) error
	// ClearFailure marks the task passed after re-inspection. When another task
	// sharing the code is still failed the block stays active (otherFailure is
	// true); otherwise every stamped row is released exactly once.
	ClearFailure(context.Context, *model.InspectionTask, uint, BlockChange) (otherFailure bool, err error)
	// ActiveFailure reports the earliest still-failed task for a component code
	// so services can reject release/approval while the block is in force.
	ActiveFailure(context.Context, string) (bool, string, string, error)
}

type airworthinessBlockRepository struct {
	db *gorm.DB
}

func NewAirworthinessBlockRepository(db *gorm.DB) AirworthinessBlockRepository {
	return &airworthinessBlockRepository{db: db}
}

// rowLock serializes failure and recovery for one component code on database
// engines that support SELECT ... FOR UPDATE. SQLite is single-writer and the
// connection pool is pinned to one connection, so an explicit lock is useless
// there and would be a syntax error.
func rowLock(tx *gorm.DB) clause.Expression {
	if tx.Dialector.Name() == "sqlite" {
		return nil
	}
	return clause.Locking{Strength: "UPDATE"}
}

func (r *airworthinessBlockRepository) ApplyFailure(ctx context.Context, task *model.InspectionTask, expectedVersion uint, change BlockChange) error {
	code := task.RelatedCode
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var group []model.InspectionTask
		query := tx.Where("related_code = ? AND deleted_at IS NULL", code)
		if lock := rowLock(tx); lock != nil {
			query = query.Clauses(lock)
		}
		if err := query.Order("id").Find(&group).Error; err != nil {
			return err
		}

		result := tx.Model(&model.InspectionTask{}).
			Where("id = ? AND version = ? AND status = ? AND deleted_at IS NULL", task.ID, expectedVersion, "running").
			Updates(map[string]any{
				"status":         "failed",
				"failure_reason": change.Reason,
				"version":        expectedVersion + 1,
				"updated_at":     change.OccurredAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrVersionConflict
		}

		if err := appendAudit(tx, change.Actor, change.RequestID, "transition", "InspectionTask", task.ID, task.Status, "failed", change.Reason); err != nil {
			return err
		}
		if err := blockParts(tx, code, change); err != nil {
			return err
		}
		return blockAuthorizations(tx, code, change)
	})
}

func (r *airworthinessBlockRepository) ClearFailure(ctx context.Context, task *model.InspectionTask, expectedVersion uint, change BlockChange) (bool, error) {
	code := task.RelatedCode
	otherFailure := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var group []model.InspectionTask
		query := tx.Where("related_code = ? AND deleted_at IS NULL", code)
		if lock := rowLock(tx); lock != nil {
			query = query.Clauses(lock)
		}
		if err := query.Order("id").Find(&group).Error; err != nil {
			return err
		}

		result := tx.Model(&model.InspectionTask{}).
			Where("id = ? AND version = ? AND status = ? AND deleted_at IS NULL", task.ID, expectedVersion, "running").
			Updates(map[string]any{
				"status":         "passed",
				"failure_reason": "",
				"version":        expectedVersion + 1,
				"updated_at":     change.OccurredAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrVersionConflict
		}

		if err := appendAudit(tx, change.Actor, change.RequestID, "transition", "InspectionTask", task.ID, "running", "passed", change.Reason); err != nil {
			return err
		}

		var stillFailed int64
		if err := tx.Model(&model.InspectionTask{}).
			Where("related_code = ? AND failure_reason <> ? AND id <> ? AND deleted_at IS NULL", code, "", task.ID).
			Count(&stillFailed).Error; err != nil {
			return err
		}
		if stillFailed > 0 {
			// The re-inspection passed, but another failed task keeps the same
			// component code blocked; stamped rows must not be released.
			otherFailure = true
			return nil
		}

		if err := releaseParts(tx, code, change); err != nil {
			return err
		}
		return releaseAuthorizations(tx, code, change)
	})
	return otherFailure, err
}

func (r *airworthinessBlockRepository) ActiveFailure(ctx context.Context, relatedCode string) (bool, string, string, error) {
	relatedCode = strings.ToUpper(strings.TrimSpace(relatedCode))
	if relatedCode == "" {
		return false, "", "", nil
	}
	var task model.InspectionTask
	// A task keeps an uncleared failure reason from the moment it fails until
	// re-inspection passes (failed -> running retains it), so the block stays in
	// force throughout the re-inspection window and not only in the failed state.
	result := r.db.WithContext(ctx).
		Where("related_code = ? AND failure_reason <> ? AND deleted_at IS NULL", relatedCode, "").
		Order("id").Limit(1).Find(&task)
	if result.Error != nil {
		return false, "", "", result.Error
	}
	if result.RowsAffected == 0 {
		return false, "", "", nil
	}
	return true, task.Code, strings.TrimSpace(task.FailureReason), nil
}

func blockParts(tx *gorm.DB, code string, change BlockChange) error {
	if code == "" {
		return nil
	}
	var parts []model.AircraftPart
	query := tx.Where("related_code = ? AND status <> ? AND deleted_at IS NULL", code, "retired")
	if lock := rowLock(tx); lock != nil {
		query = query.Clauses(lock)
	}
	if err := query.Order("id").Find(&parts).Error; err != nil {
		return err
	}
	for _, part := range parts {
		if part.BlockActive {
			// Repeated or concurrent failure judgment: the block was already
			// applied, so do not bump the version or write another audit row.
			continue
		}
		before := part.Status
		target := before
		if before != "hold" {
			target = "hold"
		}
		result := tx.Model(&model.AircraftPart{}).
			Where("id = ? AND version = ? AND block_active = ? AND deleted_at IS NULL", part.ID, part.Version, false).
			Updates(map[string]any{
				"status":             target,
				"blocking_task_code": change.TaskCode,
				"blocking_reason":    change.Reason,
				"block_active":       true,
				"version":            part.Version + 1,
				"updated_at":         change.OccurredAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		detail := "inspection " + change.TaskCode + " failed: " + change.Reason
		if err := appendAudit(tx, change.Actor, change.RequestID, "airworthiness_block", "AircraftPart", part.ID, before, target, detail); err != nil {
			return err
		}
	}
	return nil
}

func blockAuthorizations(tx *gorm.DB, code string, change BlockChange) error {
	if code == "" {
		return nil
	}
	var authorizations []model.ReleaseAuthorization
	query := tx.Where("related_code = ? AND status IN ? AND deleted_at IS NULL", code, []string{"review", "approved"})
	if lock := rowLock(tx); lock != nil {
		query = query.Clauses(lock)
	}
	if err := query.Order("id").Find(&authorizations).Error; err != nil {
		return err
	}
	for _, authorization := range authorizations {
		if authorization.BlockActive {
			continue
		}
		before := authorization.Status
		result := tx.Model(&model.ReleaseAuthorization{}).
			Where("id = ? AND version = ? AND block_active = ? AND deleted_at IS NULL", authorization.ID, authorization.Version, false).
			Updates(map[string]any{
				"status":             "restricted",
				"blocking_task_code": change.TaskCode,
				"blocking_reason":    change.Reason,
				"block_active":       true,
				"version":            authorization.Version + 1,
				"updated_at":         change.OccurredAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: authorization.ID, Version: authorization.Version + 1, Status: "restricted",
			Evidence: authorization.Evidence, Actor: change.Actor, RequestID: change.RequestID,
			Action: "airworthiness_block", Reason: "inspection " + change.TaskCode + " failed: " + change.Reason,
			CreatedAt: change.OccurredAt,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		detail := "inspection " + change.TaskCode + " failed; release forced to restricted: " + change.Reason
		if err := appendAudit(tx, change.Actor, change.RequestID, "airworthiness_block", "ReleaseAuthorization", authorization.ID, before, "restricted", detail); err != nil {
			return err
		}
	}
	return nil
}

func releaseParts(tx *gorm.DB, code string, change BlockChange) error {
	if code == "" {
		return nil
	}
	var parts []model.AircraftPart
	query := tx.Where("related_code = ? AND block_active = ? AND deleted_at IS NULL", code, true)
	if lock := rowLock(tx); lock != nil {
		query = query.Clauses(lock)
	}
	if err := query.Order("id").Find(&parts).Error; err != nil {
		return err
	}
	for _, part := range parts {
		result := tx.Model(&model.AircraftPart{}).
			Where("id = ? AND version = ? AND block_active = ? AND deleted_at IS NULL", part.ID, part.Version, true).
			Updates(map[string]any{
				"block_active": false,
				"version":      part.Version + 1,
				"updated_at":   change.OccurredAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		detail := "re-inspection of " + change.TaskCode + " passed; airworthiness block released"
		if err := appendAudit(tx, change.Actor, change.RequestID, "airworthiness_release", "AircraftPart", part.ID, part.Status, part.Status, detail); err != nil {
			return err
		}
	}
	return nil
}

func releaseAuthorizations(tx *gorm.DB, code string, change BlockChange) error {
	if code == "" {
		return nil
	}
	var authorizations []model.ReleaseAuthorization
	query := tx.Where("related_code = ? AND block_active = ? AND deleted_at IS NULL", code, true)
	if lock := rowLock(tx); lock != nil {
		query = query.Clauses(lock)
	}
	if err := query.Order("id").Find(&authorizations).Error; err != nil {
		return err
	}
	for _, authorization := range authorizations {
		result := tx.Model(&model.ReleaseAuthorization{}).
			Where("id = ? AND version = ? AND block_active = ? AND deleted_at IS NULL", authorization.ID, authorization.Version, true).
			Updates(map[string]any{
				"block_active": false,
				"version":      authorization.Version + 1,
				"updated_at":   change.OccurredAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		revision := model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: authorization.ID, Version: authorization.Version + 1, Status: authorization.Status,
			Evidence: authorization.Evidence, Actor: change.Actor, RequestID: change.RequestID,
			Action: "airworthiness_release", Reason: "re-inspection of " + change.TaskCode + " passed; block released, resubmit for review",
			CreatedAt: change.OccurredAt,
		}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		detail := "re-inspection of " + change.TaskCode + " passed; authorization may be resubmitted for review"
		if err := appendAudit(tx, change.Actor, change.RequestID, "airworthiness_release", "ReleaseAuthorization", authorization.ID, authorization.Status, authorization.Status, detail); err != nil {
			return err
		}
	}
	return nil
}
