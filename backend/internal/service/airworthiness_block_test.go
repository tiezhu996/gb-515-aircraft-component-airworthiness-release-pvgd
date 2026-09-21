package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/config"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/dto"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type closureEnv struct {
	db          *gorm.DB
	parts       AircraftPartService
	inspections InspectionTaskService
	auths       ReleaseAuthorizationService
	blocks      repository.AirworthinessBlockRepository
	blocker     AirworthinessBlocker
}

func newClosureEnv(t *testing.T) closureEnv {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_txlock=immediate"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&model.User{}, &model.AuditLog{}, &model.AircraftPart{}, &model.InspectionTask{},
		&model.ReleaseAuthorization{}, &model.ReleaseAuthorizationRevision{},
	); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	security := NewSecurityService(repository.NewSecurityRepository(db), config.Config{})
	blockRepo := repository.NewAirworthinessBlockRepository(db)
	blocker := NewAirworthinessBlockerService(blockRepo)
	return closureEnv{
		db:          db,
		parts:       NewAircraftPartService(repository.NewAircraftPartRepository(db), blocker, security),
		inspections: NewInspectionTaskService(repository.NewInspectionTaskRepository(db), blockRepo, security),
		auths:       NewReleaseAuthorizationService(repository.NewReleaseAuthorizationRepository(db), blocker, security),
		blocks:      blockRepo,
		blocker:     blocker,
	}
}

func closurePartInput(code, related string) dto.CreateAircraftPart {
	return dto.CreateAircraftPart{
		Code: code, Name: "Blocked component", Description: "closure part",
		Facility: "Hangar 2", Owner: "Airworthiness team", Category: "engine",
		RiskLevel: "high", MetricValue: 100, MetricUnit: "percent",
		EffectiveAt: time.Now().UTC(), Evidence: "base evidence", RelatedCode: related,
	}
}

func closureInspectionInput(code, related string) dto.CreateInspectionTask {
	return dto.CreateInspectionTask{
		Code: code, Name: "Blocked inspection", Description: "closure inspection",
		Facility: "Hangar 2", Owner: "Airworthiness team", Category: "engine",
		RiskLevel: "high", MetricValue: 100, MetricUnit: "percent",
		EffectiveAt: time.Now().UTC(), Evidence: "measurement sheet", RelatedCode: related,
	}
}

func closureAuthInput(code, related string) dto.CreateReleaseAuthorization {
	return dto.CreateReleaseAuthorization{
		Code: code, Name: "Blocked release", Description: "closure authorization",
		Facility: "Hangar 2", Owner: "Release desk", Category: "engine",
		RiskLevel: "high", MetricValue: 100, MetricUnit: "percent",
		EffectiveAt: time.Now().UTC(), Evidence: "evidence pack", RelatedCode: related,
	}
}

func transitionReq(status string, version uint, reason string) dto.TransitionRequest {
	return dto.TransitionRequest{Status: status, ExpectedVersion: version, Reason: reason}
}

// TestFailureBlockClosureCascadesAndGuards covers the core requirement: a
// failed inspection puts same-code parts on hold, forces review/approved
// authorizations to restricted with the failing task code and reason, and then
// blocks any release or approval until re-inspection passes.
func TestFailureBlockClosureCascadesAndGuards(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()
	const related = "REL-CLOSURE-1"
	const failReason = "crack found beyond limit on blade root"

	part, err := env.parts.Create(ctx, closurePartInput("AP-CL-1", related), "operator", "t-part")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := env.parts.Transition(ctx, part.ID, transitionReq("inspection", part.Version, "enter inspection"), "operator", "t-part-inspection"); err != nil {
		t.Fatalf("part -> inspection: %v", err)
	}
	retired, err := env.parts.Create(ctx, closurePartInput("AP-CL-RET", related), "operator", "t-part-ret")
	if err != nil {
		t.Fatalf("create retired part: %v", err)
	}
	for _, target := range []string{"inspection", "hold", "released", "retired"} {
		latest, err := env.parts.Get(ctx, retired.ID)
		if err != nil {
			t.Fatalf("reload retired part: %v", err)
		}
		if _, err := env.parts.Transition(ctx, retired.ID, transitionReq(target, latest.Version, "move to "+target), "operator", "r-move-"+target); err != nil {
			t.Fatalf("retired part -> %s: %v", target, err)
		}
	}

	inspection, err := env.inspections.Create(ctx, closureInspectionInput("IT-CL-1", related), "operator", "t-it")
	if err != nil {
		t.Fatalf("create inspection: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("running", inspection.Version, "start inspection"), "operator", "t-it-start"); err != nil {
		t.Fatalf("inspection -> running: %v", err)
	}

	reviewAuth, err := env.auths.Create(ctx, closureAuthInput("RA-CL-REVIEW", related), "operator", "t-auth-review")
	if err != nil {
		t.Fatalf("create review auth: %v", err)
	}
	if _, err := env.auths.Transition(ctx, reviewAuth.ID, transitionReq("review", reviewAuth.Version, "submit"), "operator", model.RoleOperator, "t-auth-submit"); err != nil {
		t.Fatalf("auth submit: %v", err)
	}

	approvedAuth, err := env.auths.Create(ctx, closureAuthInput("RA-CL-APPROVED", related), "operator", "t-auth-app")
	if err != nil {
		t.Fatalf("create approved auth: %v", err)
	}
	approvedAuth, err = env.auths.Transition(ctx, approvedAuth.ID, transitionReq("review", approvedAuth.Version, "submit"), "operator", model.RoleOperator, "t-auth-app-submit")
	if err != nil {
		t.Fatalf("approved auth submit: %v", err)
	}
	approvedAuth, err = env.auths.Transition(ctx, approvedAuth.ID, transitionReq("approved", approvedAuth.Version, "independent approval"), "reviewer", model.RoleReviewer, "t-auth-app-ok")
	if err != nil {
		t.Fatalf("approved auth approve: %v", err)
	}

	draftAuth, err := env.auths.Create(ctx, closureAuthInput("RA-CL-DRAFT", related), "operator", "t-auth-draft")
	if err != nil {
		t.Fatalf("create draft auth: %v", err)
	}

	running, err := env.inspections.Get(ctx, inspection.ID)
	if err != nil {
		t.Fatalf("reload running: %v", err)
	}
	failed, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("failed", running.Version, failReason), "operator", "t-it-fail")
	if err != nil {
		t.Fatalf("inspection fail: %v", err)
	}
	if failed.Status != "failed" || failed.FailureReason != failReason {
		t.Fatalf("unexpected failed inspection: %#v", failed)
	}

	blockedPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if blockedPart.Status != "hold" || !blockedPart.BlockActive || blockedPart.BlockingTaskCode != "IT-CL-1" || blockedPart.BlockingReason != failReason {
		t.Fatalf("part not blocked correctly: %#v", blockedPart)
	}

	stampedReview, err := env.auths.Get(ctx, reviewAuth.ID)
	if err != nil {
		t.Fatalf("reload review auth: %v", err)
	}
	if stampedReview.Status != "restricted" || !stampedReview.BlockActive || stampedReview.BlockingTaskCode != "IT-CL-1" || stampedReview.BlockingReason != failReason {
		t.Fatalf("review auth not restricted by failure: %#v", stampedReview)
	}
	lastRevision := stampedReview.Revisions[len(stampedReview.Revisions)-1]
	if lastRevision.Action != "airworthiness_block" || lastRevision.Reason == "" {
		t.Fatalf("missing block revision: %#v", lastRevision)
	}

	stampedApproved, err := env.auths.Get(ctx, approvedAuth.ID)
	if err != nil {
		t.Fatalf("reload approved auth: %v", err)
	}
	if stampedApproved.Status != "restricted" || !stampedApproved.BlockActive {
		t.Fatalf("approved auth must be forced restricted: %#v", stampedApproved)
	}

	untouchedDraft, err := env.auths.Get(ctx, draftAuth.ID)
	if err != nil {
		t.Fatalf("reload draft auth: %v", err)
	}
	if untouchedDraft.Status != "draft" || untouchedDraft.BlockActive {
		t.Fatalf("draft auth must be untouched: %#v", untouchedDraft)
	}

	// Even an authorization freshly submitted for review after the failure must
	// not be approved while the block is active.
	newReview, err := env.auths.Transition(ctx, draftAuth.ID, transitionReq("review", untouchedDraft.Version, "submit after failure"), "operator", model.RoleOperator, "t-late-submit")
	if err != nil {
		t.Fatalf("submitting for review after failure may proceed: %v", err)
	}
	if _, err := env.auths.Transition(ctx, newReview.ID, transitionReq("approved", newReview.Version, "reviewer approval while blocked"), "reviewer", model.RoleReviewer, "t-late-approve"); !errors.Is(err, ErrAirworthinessHold) {
		t.Fatalf("approving a freshly submitted authorization while blocked must fail, got %v", err)
	}

	retiredPart, err := env.parts.Get(ctx, retired.ID)
	if err != nil {
		t.Fatalf("reload retired part: %v", err)
	}
	if retiredPart.Status != "retired" || retiredPart.BlockActive {
		t.Fatalf("retired part must be untouched: %#v", retiredPart)
	}

	active, code, reason, err := env.blocker.ActiveFailure(ctx, related)
	if err != nil || !active || code != "IT-CL-1" || reason != failReason {
		t.Fatalf("active failure query wrong: active=%v code=%q reason=%q err=%v", active, code, reason, err)
	}

	if _, err := env.parts.Transition(ctx, blockedPart.ID, transitionReq("released", blockedPart.Version, "try release while blocked"), "operator", "t-blocked-release"); !errors.Is(err, ErrAirworthinessHold) {
		t.Fatalf("release during block must fail with ErrAirworthinessHold, got %v", err)
	}
	if _, err := env.auths.Transition(ctx, stampedReview.ID, transitionReq("review", stampedReview.Version, "resubmit while blocked"), "operator", model.RoleOperator, "t-blocked-resubmit"); !errors.Is(err, ErrBlockStillActive) {
		t.Fatalf("resubmit while blocked must fail with ErrBlockStillActive, got %v", err)
	}
	// Approval can only be reached through review (unchanged dual-control flow),
	// and that gateway is itself blocked; the graph does not permit jumping
	// restricted -> approved directly either.
	if _, err := env.auths.Transition(ctx, stampedReview.ID, transitionReq("approved", stampedReview.Version, "approve while blocked"), "reviewer", model.RoleReviewer, "t-blocked-approve"); errors.Is(err, nil) {
		t.Fatalf("approval during block must be rejected")
	}

	// Repeated failure judgment is not even a legal graph edge.
	if _, err := env.inspections.Transition(ctx, failed.ID, transitionReq("failed", failed.Version, "duplicate"), "operator", "t-dup-fail"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate failure judgment must be rejected, got %v", err)
	}
}

// TestFailureBlockRecoversOnceAndRequiresDualControl verifies that after
// re-inspection passes the block is released exactly once, the part remains on
// hold pending manual release, and the authorization can only re-enter review
// and then pass the unchanged dual-control approval.
func TestFailureBlockRecoversOnceAndRequiresDualControl(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()
	const related = "REL-CLOSURE-2"
	const failReason = "seal leakage observed"

	part, err := env.parts.Create(ctx, closurePartInput("AP-CL-2", related), "operator", "p")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := env.parts.Transition(ctx, part.ID, transitionReq("inspection", part.Version, "inspect"), "operator", "p-i"); err != nil {
		t.Fatalf("part inspect: %v", err)
	}
	inspection, err := env.inspections.Create(ctx, closureInspectionInput("IT-CL-2", related), "operator", "i")
	if err != nil {
		t.Fatalf("create inspection: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("running", inspection.Version, "start"), "operator", "i-run"); err != nil {
		t.Fatalf("inspection start: %v", err)
	}
	auth, err := env.auths.Create(ctx, closureAuthInput("RA-CL-2", related), "operator", "a")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	auth, err = env.auths.Transition(ctx, auth.ID, transitionReq("review", auth.Version, "submit"), "operator", model.RoleOperator, "a-submit")
	if err != nil {
		t.Fatalf("auth submit: %v", err)
	}

	running, err := env.inspections.Get(ctx, inspection.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	failed, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("failed", running.Version, failReason), "operator", "i-fail")
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	blockedPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	blockedAuth, err := env.auths.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload auth: %v", err)
	}
	partVersionAfterFailure := blockedPart.Version
	authVersionAfterFailure := blockedAuth.Version

	// Reopen for re-inspection: block remains active while running.
	reopened, err := env.inspections.Transition(ctx, failed.ID, transitionReq("running", failed.Version, "re-inspection opened"), "operator", "i-reopen")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := env.blocker.EnsureClear(ctx, related); !errors.Is(err, ErrBlockStillActive) {
		t.Fatalf("block must remain while re-inspection runs, got %v", err)
	}

	// Re-inspection passes: failure stamp inactivated exactly once.
	passed, err := env.inspections.Transition(ctx, reopened.ID, transitionReq("passed", reopened.Version, "re-inspection passed"), "operator", "i-pass")
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if passed.Status != "passed" || passed.FailureReason != "" {
		t.Fatalf("passed inspection should clear failure reason: %#v", passed)
	}
	if err := env.blocker.EnsureClear(ctx, related); err != nil {
		t.Fatalf("block should be clear after re-inspection: %v", err)
	}

	recoveredPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if recoveredPart.BlockActive || recoveredPart.Status != "hold" {
		t.Fatalf("part block flag must clear but part stays on hold: %#v", recoveredPart)
	}
	if recoveredPart.Version != partVersionAfterFailure+1 {
		t.Fatalf("part recovery must bump version exactly once: %d vs %d", recoveredPart.Version, partVersionAfterFailure+1)
	}
	if recoveredPart.BlockingTaskCode != "IT-CL-2" || recoveredPart.BlockingReason != failReason {
		t.Fatalf("block stamp must remain for traceability: %#v", recoveredPart)
	}

	// A redundant recovery call cannot take effect again: stamped rows keep their version.
	otherFailure, err := env.blocks.ClearFailure(ctx, &model.InspectionTask{BaseModel: model.BaseModel{ID: passed.ID, Version: passed.Version, Status: passed.Status}, RelatedCode: related}, passed.Version, repository.BlockChange{Actor: "operator", RequestID: "dup-recover", TaskCode: passed.Code, OccurredAt: time.Now().UTC()})
	if err == nil {
		t.Fatalf("clearing an already passed task must conflict, otherFailure=%v", otherFailure)
	}

	recoveredAuth, err := env.auths.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload auth: %v", err)
	}
	if recoveredAuth.BlockActive || recoveredAuth.Status != "restricted" {
		t.Fatalf("auth block flag must clear but status stays restricted: %#v", recoveredAuth)
	}
	if recoveredAuth.Version != authVersionAfterFailure+1 {
		t.Fatalf("auth recovery must bump version exactly once: %d vs %d", recoveredAuth.Version, authVersionAfterFailure+1)
	}

	// Passing re-inspection only allows resubmission; approval remains dual control.
	resubmitted, err := env.auths.Transition(ctx, auth.ID, transitionReq("review", recoveredAuth.Version, "resubmit after re-inspection"), "operator", model.RoleOperator, "a-resubmit")
	if err != nil {
		t.Fatalf("resubmit after recovery: %v", err)
	}
	if resubmitted.Status != "review" || resubmitted.SubmittedBy != "operator" || resubmitted.BlockingTaskCode != "" || resubmitted.BlockingReason != "" {
		t.Fatalf("unexpected resubmission: %#v", resubmitted)
	}
	if _, err := env.auths.Transition(ctx, resubmitted.ID, transitionReq("approved", resubmitted.Version, "self approval"), "operator", model.RoleReviewer, "a-self-approve"); !errors.Is(err, ErrSeparationOfDuty) {
		t.Fatalf("same-user re-approval must be rejected, got %v", err)
	}
	approved, err := env.auths.Transition(ctx, resubmitted.ID, transitionReq("approved", resubmitted.Version, "independent re-approval"), "reviewer", model.RoleReviewer, "a-reapprove")
	if err != nil {
		t.Fatalf("independent re-approval: %v", err)
	}
	if approved.Status != "approved" || approved.ReviewedBy != "reviewer" {
		t.Fatalf("unexpected final approval: %#v", approved)
	}

	// If the reviewer restricts again after a later re-submission, that
	// restriction is a fresh reviewer decision and must not reopen the
	// failure-stamp gateway (the stamp was cleared on resubmission).
	againDraft, err := env.auths.Transition(ctx, approved.ID, transitionReq("restricted", approved.Version, "reviewer finds new issue"), "reviewer", model.RoleReviewer, "a-rerestrict")
	if err != nil {
		t.Fatalf("re-restrict: %v", err)
	}
	if againDraft.BlockingTaskCode != "" || againDraft.BlockActive {
		t.Fatalf("reviewer restriction must not carry a failure stamp: %#v", againDraft)
	}
	if _, err := env.auths.Transition(ctx, againDraft.ID, transitionReq("review", againDraft.Version, "reopen manual restriction"), "operator", model.RoleOperator, "a-reopen-manual"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a reviewer re-restriction must not re-enter review via the failure gateway, got %v", err)
	}

	// Part still needs an explicit manual release, which is now allowed.
	if _, err := env.parts.Transition(ctx, recoveredPart.ID, transitionReq("released", recoveredPart.Version, "manual release after recovery"), "operator", "p-release"); err != nil {
		t.Fatalf("manual release after recovery should succeed: %v", err)
	}
}

// TestConcurrentFailureJudgmentAppliesOnce proves that two racing failure
// judgments cannot double-apply the cascade or leave a half update.
func TestConcurrentFailureJudgmentAppliesOnce(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()
	const related = "REL-CLOSURE-3"

	part, err := env.parts.Create(ctx, closurePartInput("AP-CL-3", related), "operator", "p")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := env.parts.Transition(ctx, part.ID, transitionReq("inspection", part.Version, "inspect"), "operator", "p-i"); err != nil {
		t.Fatalf("part inspect: %v", err)
	}
	inspection, err := env.inspections.Create(ctx, closureInspectionInput("IT-CL-3", related), "operator", "i")
	if err != nil {
		t.Fatalf("create inspection: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("running", inspection.Version, "start"), "operator", "i-run"); err != nil {
		t.Fatalf("inspection start: %v", err)
	}
	running, err := env.inspections.Get(ctx, inspection.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, results[index] = env.inspections.Transition(ctx, inspection.ID, transitionReq("failed", running.Version, "concurrent judgment"), "operator", "concurrent-fail")
		}(index)
	}
	close(start)
	wg.Wait()

	successes, rejected := 0, 0
	for _, result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, repository.ErrVersionConflict), errors.Is(result, ErrInvalidTransition):
			// Depending on read timing the loser is stopped by the optimistic CAS
			// or by the graph once the winner has already committed.
			rejected++
		default:
			t.Fatalf("unexpected concurrent result: %v", result)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("expected exactly one success and one rejection, got %d/%d (results=%v)", successes, rejected, results)
	}

	finalPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if finalPart.Status != "hold" || !finalPart.BlockActive || finalPart.Version != 3 {
		t.Fatalf("part cascade must apply exactly once: %#v", finalPart)
	}
	var blockAudits int64
	if err := env.db.Model(&model.AuditLog{}).
		Where("entity_type = ? AND entity_id = ? AND action = ?", "AircraftPart", part.ID, "airworthiness_block").
		Count(&blockAudits).Error; err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if blockAudits != 1 {
		t.Fatalf("expected one block audit, got %d", blockAudits)
	}
}

// TestApplyFailureCASIsIdempotent proves the repository-level compare-and-swap
// applies the whole cascade once even when two requests hold the same
// pre-failure snapshot, on every supported database driver.
func TestApplyFailureCASIsIdempotent(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()
	const related = "REL-CAS-1"

	part, err := env.parts.Create(ctx, closurePartInput("AP-CAS-1", related), "operator", "p")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	inspection, err := env.inspections.Create(ctx, closureInspectionInput("IT-CAS-1", related), "operator", "i")
	if err != nil {
		t.Fatalf("create inspection: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("running", inspection.Version, "start"), "operator", "run"); err != nil {
		t.Fatalf("start: %v", err)
	}
	running, err := env.inspections.Get(ctx, inspection.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	snapshot := running
	change := repository.BlockChange{Actor: "operator", RequestID: "cas-1", TaskCode: running.Code, Reason: "first failure", OccurredAt: time.Now().UTC()}
	if err := env.blocks.ApplyFailure(ctx, &snapshot, running.Version, change); err != nil {
		t.Fatalf("first failure must apply: %v", err)
	}
	duplicate := running
	if err := env.blocks.ApplyFailure(ctx, &duplicate, running.Version, change); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("second failure against the same version must conflict, got %v", err)
	}

	finalPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if finalPart.Version != 2 || !finalPart.BlockActive || finalPart.BlockingReason != "first failure" {
		t.Fatalf("cascade must apply exactly once: %#v", finalPart)
	}
}

// TestOneFailedTaskKeepsBlockWhileAnotherRecovers covers multiple inspection
// tasks sharing one component code: recovery of one failed task must not lift
// the block while another task is still failed.
func TestOneFailedTaskKeepsBlockWhileAnotherRecovers(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()
	const related = "REL-CLOSURE-4"

	part, err := env.parts.Create(ctx, closurePartInput("AP-CL-4", related), "operator", "p")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	first, err := env.inspections.Create(ctx, closureInspectionInput("IT-CL-4A", related), "operator", "ia")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := env.inspections.Create(ctx, closureInspectionInput("IT-CL-4B", related), "operator", "ib")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	auth, err := env.auths.Create(ctx, closureAuthInput("RA-CL-4", related), "operator", "a")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	auth, err = env.auths.Transition(ctx, auth.ID, transitionReq("review", auth.Version, "submit"), "operator", model.RoleOperator, "a-submit")
	if err != nil {
		t.Fatalf("submit auth: %v", err)
	}

	failOne := func(id uint, version uint, reason string) uint {
		t.Helper()
		failed, err := env.inspections.Transition(ctx, id, transitionReq("failed", version, reason), "operator", "fail")
		if err != nil {
			t.Fatalf("fail task %d: %v", id, err)
		}
		return failed.Version
	}
	first, _ = env.inspections.Get(ctx, first.ID)
	if _, err := env.inspections.Transition(ctx, first.ID, transitionReq("running", first.Version, "start"), "operator", "run-a"); err != nil {
		t.Fatalf("start a: %v", err)
	}
	second, _ = env.inspections.Get(ctx, second.ID)
	if _, err := env.inspections.Transition(ctx, second.ID, transitionReq("running", second.Version, "start"), "operator", "run-b"); err != nil {
		t.Fatalf("start b: %v", err)
	}
	first, _ = env.inspections.Get(ctx, first.ID)
	vFailA := failOne(first.ID, first.Version, "first failure")
	second, _ = env.inspections.Get(ctx, second.ID)
	failOne(second.ID, second.Version, "second failure")

	blockedPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if !blockedPart.BlockActive {
		t.Fatalf("part must be blocked after first failure")
	}
	partVersionBlocked := blockedPart.Version

	// First task re-inspects and passes; second failure keeps the block.
	reopenA, err := env.inspections.Transition(ctx, first.ID, transitionReq("running", vFailA, "reopen a"), "operator", "reopen-a")
	if err != nil {
		t.Fatalf("reopen a: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, first.ID, transitionReq("passed", reopenA.Version, "a passes"), "operator", "pass-a"); err != nil {
		t.Fatalf("pass a: %v", err)
	}
	stillBlocked, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if !stillBlocked.BlockActive || stillBlocked.Version != partVersionBlocked {
		t.Fatalf("block must remain and must not touch part while another failure exists: %#v", stillBlocked)
	}
	blockedAuth, err := env.auths.Get(ctx, auth.ID)
	if err != nil {
		t.Fatalf("reload auth: %v", err)
	}
	if _, err := env.auths.Transition(ctx, blockedAuth.ID, transitionReq("review", blockedAuth.Version, "resubmit early"), "operator", model.RoleOperator, "early-resubmit"); !errors.Is(err, ErrBlockStillActive) {
		t.Fatalf("resubmit must stay blocked while second task failed, got %v", err)
	}

	// Second task passes: now the block lifts exactly once and resubmission works.
	secondFailed, err := env.inspections.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("reload b: %v", err)
	}
	reopenB, err := env.inspections.Transition(ctx, second.ID, transitionReq("running", secondFailed.Version, "reopen b"), "operator", "reopen-b")
	if err != nil {
		t.Fatalf("reopen b: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, second.ID, transitionReq("passed", reopenB.Version, "b passes"), "operator", "pass-b"); err != nil {
		t.Fatalf("pass b: %v", err)
	}
	recovered, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if recovered.BlockActive || recovered.Version != partVersionBlocked+1 {
		t.Fatalf("block must release once after last failure clears: %#v", recovered)
	}
}

// TestReviewerRestrictionCannotResubmit ensures a restriction imposed by a
// reviewer (no failure stamp) keeps using the original restricted lifecycle.
func TestReviewerRestrictionCannotResubmit(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()

	auth, err := env.auths.Create(ctx, closureAuthInput("RA-MANUAL", "REL-MANUAL"), "operator", "a")
	if err != nil {
		t.Fatalf("create auth: %v", err)
	}
	auth, err = env.auths.Transition(ctx, auth.ID, transitionReq("review", auth.Version, "submit"), "operator", model.RoleOperator, "submit")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	auth, err = env.auths.Transition(ctx, auth.ID, transitionReq("restricted", auth.Version, "reviewer limits release"), "reviewer", model.RoleReviewer, "restrict")
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	if auth.BlockActive || auth.BlockingTaskCode != "" {
		t.Fatalf("reviewer restriction must not carry a failure stamp: %#v", auth)
	}
	if _, err := env.auths.Transition(ctx, auth.ID, transitionReq("review", auth.Version, "reopen manual restriction"), "operator", model.RoleOperator, "reopen-manual"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("manual restricted authorization must not re-enter review, got %v", err)
	}
}

// TestNormalPassHasNoCascade ensures an inspection that never failed performs no
// blocking writes.
func TestNormalPassHasNoCascade(t *testing.T) {
	env := newClosureEnv(t)
	ctx := context.Background()
	const related = "REL-CLEAN"

	part, err := env.parts.Create(ctx, closurePartInput("AP-CLEAN", related), "operator", "p")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	inspection, err := env.inspections.Create(ctx, closureInspectionInput("IT-CLEAN", related), "operator", "i")
	if err != nil {
		t.Fatalf("create inspection: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("running", inspection.Version, "start"), "operator", "run"); err != nil {
		t.Fatalf("start: %v", err)
	}
	running, err := env.inspections.Get(ctx, inspection.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := env.inspections.Transition(ctx, inspection.ID, transitionReq("passed", running.Version, "all checks pass"), "operator", "pass"); err != nil {
		t.Fatalf("pass: %v", err)
	}
	cleanPart, err := env.parts.Get(ctx, part.ID)
	if err != nil {
		t.Fatalf("reload part: %v", err)
	}
	if cleanPart.Status != "received" || cleanPart.BlockActive || cleanPart.BlockingTaskCode != "" || cleanPart.Version != 1 {
		t.Fatalf("a clean pass must not cascade to the part: %#v", cleanPart)
	}
}
