package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const blockTestCode = "REL-BLOCK-01"

type blockFixtures struct {
	db             *gorm.DB
	part           model.AircraftPart
	inspection     model.InspectionTask
	authorization  model.ReleaseAuthorization
	inspections    *inspectionTaskService
	parts          *aircraftPartService
	authorizations *releaseAuthorizationService
}

func newBlockTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.AuditLog{},
		&model.AircraftPart{},
		&model.InspectionTask{},
		&model.ReleaseAuthorization{},
		&model.ReleaseAuthorizationRevision{},
	); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}

func setupBlockFixtures(t *testing.T, startAuthStatus string) blockFixtures {
	t.Helper()
	db := newBlockTestDB(t)
	now := time.Now().UTC()
	part := model.AircraftPart{
		BaseModel: model.BaseModel{Code: "AP-BLOCK-1", Name: "Blocked part", Status: "inspection", Version: 1},
		Facility:  "Hangar 1", Owner: "Release desk", Category: "engine", RiskLevel: "high",
		MetricValue: 1, MetricUnit: "unit", EffectiveAt: now, Evidence: "evidence", RelatedCode: blockTestCode,
	}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create part: %v", err)
	}
	inspection := model.InspectionTask{
		BaseModel: model.BaseModel{Code: "IT-BLOCK-1", Name: "Failing inspection", Status: "running", Version: 1},
		Facility:  "Hangar 1", Owner: "Release desk", Category: "engine", RiskLevel: "high",
		MetricValue: 1, MetricUnit: "unit", EffectiveAt: now, Evidence: "evidence", RelatedCode: blockTestCode,
	}
	if err := db.Create(&inspection).Error; err != nil {
		t.Fatalf("create inspection: %v", err)
	}
	authorization := model.ReleaseAuthorization{
		BaseModel: model.BaseModel{Code: "RA-BLOCK-1", Name: "Blocked authorization", Status: startAuthStatus, Version: 3},
		Facility:  "Hangar 1", Owner: "Release desk", Category: "engine", RiskLevel: "high",
		MetricValue: 1, MetricUnit: "unit", EffectiveAt: now, Evidence: "evidence", RelatedCode: blockTestCode,
		SubmittedBy: "operator", ReviewedBy: "reviewer", ReviewReason: "independent review passed",
	}
	if err := db.Create(&authorization).Error; err != nil {
		t.Fatalf("create authorization: %v", err)
	}
	blockingRepo := repository.NewAirworthinessBlockRepository(db)
	return blockFixtures{
		db: db, part: part, inspection: inspection, authorization: authorization,
		inspections:    NewInspectionTaskService(repository.NewInspectionTaskRepository(db), blockingRepo, nil).(*inspectionTaskService),
		parts:          NewAircraftPartService(repository.NewAircraftPartRepository(db), nil).(*aircraftPartService),
		authorizations: NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), blockingRepo, nil).(*releaseAuthorizationService),
	}
}

func failInspection(t *testing.T, fixtures blockFixtures) (model.InspectionTask, model.AircraftPart, model.ReleaseAuthorization) {
	t.Helper()
	ctx := context.Background()
	reason := "无损检测发现超标裂纹"
	_, err := fixtures.inspections.Transition(ctx, fixtures.inspection.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: fixtures.inspection.Version, Reason: reason,
	}, "operator", "inspection-failed-1")
	if err != nil {
		t.Fatalf("fail inspection: %v", err)
	}
	failedTask, err := fixtures.inspections.Get(ctx, fixtures.inspection.ID)
	if err != nil {
		t.Fatalf("reload inspection: %v", err)
	}
	if failedTask.Status != "failed" || failedTask.FailureReason != reason {
		t.Fatalf("inspection not failed with reason: %#v", failedTask)
	}
	part, err := fixtures.parts.Get(ctx, fixtures.part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	authorization, err := fixtures.authorizations.Get(ctx, fixtures.authorization.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	return failedTask, part, authorization
}

func TestFailureCascadePausesPartAndRestrictsAuthorizationAtomically(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	_, part, authorization := failInspection(t, fixtures)

	if part.Status != "hold" || part.PreBlockStatus != "inspection" {
		t.Fatalf("part must be paused from inspection, got %#v", part)
	}
	if !part.Blocked() || part.BlockedByTaskCode != "IT-BLOCK-1" || part.BlockReason != "无损检测发现超标裂纹" || part.BlockedAt == nil {
		t.Fatalf("part block metadata missing: %#v", part)
	}
	if authorization.Status != "restricted" || !authorization.Blocked() {
		t.Fatalf("approved authorization must be restricted, got %#v", authorization)
	}
	if authorization.BlockedByTaskCode != "IT-BLOCK-1" || authorization.BlockedAt == nil {
		t.Fatalf("authorization block metadata missing: %#v", authorization)
	}
	if len(authorization.Revisions) != 1 {
		t.Fatalf("expected one blocking revision, got %d", len(authorization.Revisions))
	}
	revision := authorization.Revisions[0]
	if revision.Action != "airworthiness-block" || revision.RequestID != "inspection-failed-1" || revision.Version != 4 {
		t.Fatalf("unexpected blocking revision: %#v", revision)
	}

	// Audit chain covers task decision, part pause and authorization restriction.
	assertAirworthinessAudit(t, fixtures.db, "InspectionTask", fixtures.inspection.ID, "transition", "failed")
	assertAirworthinessAudit(t, fixtures.db, "AircraftPart", fixtures.part.ID, "airworthiness-block", "hold")
	assertAirworthinessAudit(t, fixtures.db, "ReleaseAuthorization", fixtures.authorization.ID, "airworthiness-block", "restricted")
}

func TestBlockedPartRejectsAllManualTransitions(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	_, part, _ := failInspection(t, fixtures)
	ctx := context.Background()
	for _, target := range []string{"released", "retired", "inspection"} {
		_, err := fixtures.parts.Transition(ctx, part.ID, dto.TransitionRequest{
			Status: target, ExpectedVersion: part.Version, Reason: "attempt manual move while blocked",
		}, "operator", "part-escape")
		if !errors.Is(err, ErrAirworthinessHold) {
			t.Fatalf("part transition %s must be blocked, got %v", target, err)
		}
	}
}

func TestBlockedAuthorizationCannotBeApprovedOrResubmitted(t *testing.T) {
	fixtures := setupBlockFixtures(t, "review")
	_, _, authorization := failInspection(t, fixtures)
	ctx := context.Background()

	// Operator cannot resubmit a still-active block.
	if _, err := fixtures.authorizations.Transition(ctx, authorization.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: authorization.Version, Reason: "try to resubmit",
	}, "operator", model.RoleOperator, "auth-resubmit-too-early"); !errors.Is(err, ErrAirworthinessHold) {
		t.Fatalf("resubmit during active block must fail, got %v", err)
	}

	// A new authorization submitted for review while the failed inspection is
	// still open cannot be approved either: failure closes every release.
	draft := model.ReleaseAuthorization{
		BaseModel: model.BaseModel{Code: "RA-BLOCK-2", Name: "Late authorization", Status: "draft", Version: 1},
		Facility:  "Hangar 1", Owner: "Release desk", Category: "engine", RiskLevel: "high",
		MetricValue: 1, MetricUnit: "unit", EffectiveAt: time.Now().UTC(), Evidence: "evidence", RelatedCode: blockTestCode,
	}
	if err := fixtures.db.Create(&draft).Error; err != nil {
		t.Fatalf("create late draft: %v", err)
	}
	submitted, err := fixtures.authorizations.Transition(ctx, draft.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: draft.Version, Reason: "block open but submit",
	}, "operator", model.RoleOperator, "late-submit")
	if err != nil {
		t.Fatalf("draft submission itself is allowed: %v", err)
	}
	if _, err := fixtures.authorizations.Transition(ctx, submitted.ID, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: submitted.Version, Reason: "sneak approval",
	}, "reviewer", model.RoleReviewer, "auth-sneak-approve"); !errors.Is(err, ErrAirworthinessHold) {
		t.Fatalf("approval during active block must fail, got %v", err)
	}
}

func TestReInspectionPassResumesOnceAndRequiresResubmission(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	failedTask, part, authorization := failInspection(t, fixtures)
	ctx := context.Background()

	// Restart the failed inspection, then pass the re-inspection.
	running, err := fixtures.inspections.Transition(ctx, failedTask.ID, dto.TransitionRequest{
		Status: "running", ExpectedVersion: failedTask.Version, Reason: "开始重新检查",
	}, "operator", "inspection-rerun-1")
	if err != nil {
		t.Fatalf("restart inspection: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, running.ID, dto.TransitionRequest{
		Status: "passed", ExpectedVersion: running.Version, Reason: "重新检查合格",
	}, "operator", "inspection-passed-1"); err != nil {
		t.Fatalf("pass re-inspection: %v", err)
	}

	reloadedPart, err := fixtures.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if reloadedPart.Status != "inspection" || reloadedPart.Blocked() || reloadedPart.BlockResolvedAt == nil {
		t.Fatalf("part must resume once after pass, got %#v", reloadedPart)
	}
	reloadedAuth, err := fixtures.authorizations.Get(ctx, authorization.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if reloadedAuth.Status != "restricted" || reloadedAuth.BlockResolvedAt == nil {
		t.Fatalf("authorization stays restricted but block resolved, got %#v", reloadedAuth)
	}

	// Passed re-inspection only enables resubmission into review, never approval.
	if _, err := fixtures.authorizations.Transition(ctx, reloadedAuth.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: reloadedAuth.Version, Reason: "依据重新检查结果重新提交复核",
	}, "operator", model.RoleOperator, "auth-resubmit-1"); err != nil {
		t.Fatalf("resubmit after resolved block: %v", err)
	}
	review, err := fixtures.authorizations.Get(ctx, reloadedAuth.ID)
	if err != nil {
		t.Fatalf("reload resubmitted authorization: %v", err)
	}
	if review.Status != "review" || review.SubmittedBy != "operator" || review.ReviewedBy != "" {
		t.Fatalf("resubmission must reset dual-control attribution, got %#v", review)
	}
	// Original submitter still cannot self-approve; an independent reviewer can.
	approval := dto.TransitionRequest{Status: "approved", ExpectedVersion: review.Version, Reason: "independent re-review passed"}
	if _, err := fixtures.authorizations.Transition(ctx, review.ID, approval, "operator", model.RoleReviewer, "auth-same-user"); !errors.Is(err, ErrSeparationOfDuty) {
		t.Fatalf("same user re-approval must be rejected, got %v", err)
	}
	approved, err := fixtures.authorizations.Transition(ctx, review.ID, approval, "reviewer", model.RoleReviewer, "auth-reapprove")
	if err != nil {
		t.Fatalf("independent reviewer re-approval: %v", err)
	}
	if approved.Status != "approved" {
		t.Fatalf("expected approved after dual-control re-review, got %s", approved.Status)
	}
}

func TestRecoveryAppliesOnlyOnce(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	failedTask, part, _ := failInspection(t, fixtures)
	ctx := context.Background()

	// Manually move the part away after the block to simulate concurrent work;
	// the one-shot resume must touch it exactly once.
	if err := fixtures.db.Model(&model.AircraftPart{}).Where("id = ?", part.ID).
		Updates(map[string]interface{}{"status": "retired", "version": gorm.Expr("version + 1")}).Error; err != nil {
		t.Fatalf("simulate concurrent part move: %v", err)
	}

	running, err := fixtures.inspections.Transition(ctx, failedTask.ID, dto.TransitionRequest{
		Status: "running", ExpectedVersion: failedTask.Version, Reason: "重新检查开始",
	}, "operator", "rerun")
	if err != nil {
		t.Fatalf("restart inspection: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, running.ID, dto.TransitionRequest{
		Status: "passed", ExpectedVersion: running.Version, Reason: "重新检查合格",
	}, "operator", "pass"); err != nil {
		t.Fatalf("pass re-inspection: %v", err)
	}

	var partAfter model.AircraftPart
	if err := fixtures.db.First(&partAfter, part.ID).Error; err != nil {
		t.Fatalf("reload part: %v", err)
	}
	// Retired part is not force-restored to an active state, but its block is
	// still resolved exactly once.
	if partAfter.Status != "retired" || partAfter.BlockResolvedAt == nil {
		t.Fatalf("retired part keeps status but block must resolve, got %#v", partAfter)
	}
	var unblockAudits int64
	if err := fixtures.db.Model(&model.AuditLog{}).
		Where("entity_type = ? AND entity_id = ? AND action = ?", "AircraftPart", part.ID, "airworthiness-unblock").
		Count(&unblockAudits).Error; err != nil {
		t.Fatalf("count unblock audits: %v", err)
	}
	if unblockAudits != 1 {
		t.Fatalf("recovery must apply once, got %d unblock audits", unblockAudits)
	}
}

func TestDuplicateFailureJudgmentRejectedByOptimisticLock(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	ctx := context.Background()
	// First failure wins and cascades.
	if _, err := fixtures.inspections.Transition(ctx, fixtures.inspection.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: fixtures.inspection.Version, Reason: "首次判定失败",
	}, "operator", "first-failure"); err != nil {
		t.Fatalf("first failure: %v", err)
	}
	// Concurrent duplicate judgment carrying the stale version must not apply
	// the cascade a second time.
	_, err := fixtures.inspections.Transition(ctx, fixtures.inspection.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: fixtures.inspection.Version, Reason: "重复判定失败",
	}, "operator", "duplicate-failure")
	if !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("duplicate failure judgment must conflict, got %v", err)
	}

	var part model.AircraftPart
	if err := fixtures.db.First(&part, fixtures.part.ID).Error; err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if part.Version != fixtures.part.Version+1 || part.BlockReason != "首次判定失败" {
		t.Fatalf("duplicate failure must not reapply, got version=%d reason=%q", part.Version, part.BlockReason)
	}
	var blockAudits int64
	if err := fixtures.db.Model(&model.AuditLog{}).
		Where("entity_type = ? AND entity_id = ? AND action = ?", "AircraftPart", fixtures.part.ID, "airworthiness-block").
		Count(&blockAudits).Error; err != nil {
		t.Fatalf("count block audits: %v", err)
	}
	if blockAudits != 1 {
		t.Fatalf("part block must apply once, got %d audits", blockAudits)
	}
}

func TestSecondFailedInspectionKeepsBlockClosed(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	firstFailed, _, _ := failInspection(t, fixtures)
	ctx := context.Background()
	now := time.Now().UTC()
	second := model.InspectionTask{
		BaseModel: model.BaseModel{Code: "IT-BLOCK-2", Name: "Second failing inspection", Status: "running", Version: 1},
		Facility:  "Hangar 1", Owner: "Release desk", Category: "engine", RiskLevel: "high",
		MetricValue: 1, MetricUnit: "unit", EffectiveAt: now, Evidence: "evidence", RelatedCode: blockTestCode,
	}
	if err := fixtures.db.Create(&second).Error; err != nil {
		t.Fatalf("create second inspection: %v", err)
	}
	// Pass the first failed inspection's rerun; the second open failure keeps
	// the same-code block closed.
	running, err := fixtures.inspections.Transition(ctx, firstFailed.ID, dto.TransitionRequest{
		Status: "running", ExpectedVersion: firstFailed.Version, Reason: "重新检查",
	}, "operator", "rerun-1")
	if err != nil {
		t.Fatalf("restart first: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, second.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: second.Version, Reason: "第二次发现缺陷",
	}, "operator", "second-failure"); err != nil {
		t.Fatalf("second failure: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, running.ID, dto.TransitionRequest{
		Status: "passed", ExpectedVersion: running.Version, Reason: "第一次重新检查合格",
	}, "operator", "first-pass"); err != nil {
		t.Fatalf("pass first rerun: %v", err)
	}
	var part model.AircraftPart
	if err := fixtures.db.First(&part, fixtures.part.ID).Error; err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if part.Status != "hold" || !part.Blocked() {
		t.Fatalf("part must stay paused while another failed inspection is open, got %#v", part)
	}

	// Once the second inspection also passes, the block resolves.
	secondFailed, err := fixtures.inspections.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("reload second: %v", err)
	}
	running2, err := fixtures.inspections.Transition(ctx, secondFailed.ID, dto.TransitionRequest{
		Status: "running", ExpectedVersion: secondFailed.Version, Reason: "重新检查",
	}, "operator", "rerun-2")
	if err != nil {
		t.Fatalf("restart second: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, running2.ID, dto.TransitionRequest{
		Status: "passed", ExpectedVersion: running2.Version, Reason: "第二次重新检查合格",
	}, "operator", "second-pass"); err != nil {
		t.Fatalf("pass second rerun: %v", err)
	}
	if err := fixtures.db.First(&part, fixtures.part.ID).Error; err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if part.Blocked() || part.Status != "inspection" {
		t.Fatalf("block must resolve after all failures cleared, got %#v", part)
	}
}

func TestReviewerChosenRestrictionCannotResubmit(t *testing.T) {
	fixtures := setupBlockFixtures(t, "review")
	ctx := context.Background()
	// Reviewer restricts the authorization directly (no inspection involved).
	restricted, err := fixtures.authorizations.Transition(ctx, fixtures.authorization.ID, dto.TransitionRequest{
		Status: "restricted", ExpectedVersion: fixtures.authorization.Version, Reason: "复核员发现限制条件",
	}, "reviewer", model.RoleReviewer, "reviewer-restrict")
	if err != nil {
		t.Fatalf("reviewer restrict: %v", err)
	}
	if _, err := fixtures.authorizations.Transition(ctx, restricted.ID, dto.TransitionRequest{
		Status: "review", ExpectedVersion: restricted.Version, Reason: "尝试绕过限制",
	}, "operator", model.RoleOperator, "bypass-restrict"); !errors.Is(err, ErrAirworthinessHold) {
		t.Fatalf("reviewer restriction must remain terminal, got %v", err)
	}
}

func assertAirworthinessAudit(t *testing.T, db *gorm.DB, entityType string, entityID uint, action, after string) {
	t.Helper()
	var audit model.AuditLog
	err := db.Where("entity_type = ? AND entity_id = ? AND action = ? AND after_state = ?",
		entityType, entityID, action, after).First(&audit).Error
	if err != nil {
		t.Fatalf("expected audit %s/%s/%s: %v", entityType, action, after, err)
	}
}

func TestNewFailureReBlocksAfterRecovery(t *testing.T) {
	fixtures := setupBlockFixtures(t, "approved")
	firstFailed, _, _ := failInspection(t, fixtures)
	ctx := context.Background()

	// Clear the first failure through a passed re-inspection.
	running, err := fixtures.inspections.Transition(ctx, firstFailed.ID, dto.TransitionRequest{
		Status: "running", ExpectedVersion: firstFailed.Version, Reason: "重新检查",
	}, "operator", "rerun-1")
	if err != nil {
		t.Fatalf("restart first: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, running.ID, dto.TransitionRequest{
		Status: "passed", ExpectedVersion: running.Version, Reason: "重新检查合格",
	}, "operator", "pass-1"); err != nil {
		t.Fatalf("pass first: %v", err)
	}

	// A brand-new inspection for the same code fails afterwards: the part and
	// authorization must be blocked again, each transition single-shot.
	second := model.InspectionTask{
		BaseModel: model.BaseModel{Code: "IT-BLOCK-9", Name: "Later failing inspection", Status: "running", Version: 1},
		Facility:  "Hangar 1", Owner: "Release desk", Category: "engine", RiskLevel: "high",
		MetricValue: 1, MetricUnit: "unit", EffectiveAt: time.Now().UTC(), Evidence: "evidence", RelatedCode: blockTestCode,
	}
	if err := fixtures.db.Create(&second).Error; err != nil {
		t.Fatalf("create second inspection: %v", err)
	}
	if _, err := fixtures.inspections.Transition(ctx, second.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: 1, Reason: "复检后再次发现缺陷",
	}, "operator", "reblock"); err != nil {
		t.Fatalf("second failure re-block: %v", err)
	}
	part, err := fixtures.parts.Get(ctx, fixtures.part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if !part.Blocked() || part.Status != "hold" || part.BlockedByTaskCode != "IT-BLOCK-9" || part.BlockReason != "复检后再次发现缺陷" {
		t.Fatalf("part must be re-blocked by the new failure, got %#v", part)
	}
	authorization, err := fixtures.authorizations.Get(ctx, fixtures.authorization.ID)
	if err != nil {
		t.Fatalf("reload authorization: %v", err)
	}
	if !authorization.Blocked() || authorization.Status != "restricted" {
		t.Fatalf("authorization must be re-blocked, got %#v", authorization)
	}
}
