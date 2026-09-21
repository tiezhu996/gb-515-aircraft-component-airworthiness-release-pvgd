package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/config"
	"github.com/blueship581/aircraft-component-airworthiness-release/backend/internal/model"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Open(ctx context.Context, cfg config.Config, log *slog.Logger) (*gorm.DB, *redis.Client, error) {
	var dialector gorm.Dialector
	switch cfg.DatabaseDriver {
	case "postgres":
		dialector = postgres.Open(cfg.DatabaseDSN)
	case "mysql":
		dialector = mysql.Open(cfg.DatabaseDSN)
	case "sqlite":
		dialector = sqlite.Open(cfg.DatabaseDSN)
	default:
		return nil, nil, fmt.Errorf("unsupported database driver %q", cfg.DatabaseDriver)
	}
	logLevel := logger.Warn
	if cfg.Environment == "development" {
		logLevel = logger.Info
	}
	var db *gorm.DB
	var err error
	for attempt := 1; attempt <= 20; attempt++ {
		db, err = gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logLevel)})
		if err == nil {
			sqlDB, dbErr := db.DB()
			if dbErr == nil && sqlDB.PingContext(ctx) == nil {
				break
			}
			if dbErr != nil {
				err = dbErr
			} else {
				err = sqlDB.PingContext(ctx)
			}
		}
		log.Warn("database not ready", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("connect database: %w", err)
	}
	// SQLite permits only one writer. Pinning the pool serializes transactions
	// (the failure-block closure relies on row locks on MySQL/Postgres) and
	// prevents SQLITE_BUSY failures during concurrent requests.
	if cfg.DatabaseDriver == "sqlite" {
		sqlDB, err := db.DB()
		if err != nil {
			return nil, nil, fmt.Errorf("configure sqlite pool: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
	}
	if err := migrate(db); err != nil {
		return nil, nil, err
	}
	if err := Seed(ctx, db); err != nil {
		return nil, nil, err
	}
	var redisClient *redis.Client
	if cfg.RedisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
		if err := redisClient.Ping(ctx).Err(); err != nil {
			return nil, nil, fmt.Errorf("connect redis: %w", err)
		}
	}
	return db, redisClient, nil
}

func migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.User{}, &model.AuditLog{},
		&model.AircraftPart{},
		&model.InspectionTask{},
		&model.CertificateRecord{},
		&model.CertificateRecordRevision{},
		&model.ReleaseAuthorization{},
		&model.ReleaseAuthorizationRevision{},
	)
}

func Seed(ctx context.Context, db *gorm.DB) error {
	var users int64
	if err := db.WithContext(ctx).Model(&model.User{}).Count(&users).Error; err != nil {
		return err
	}
	if users == 0 {
		password, err := bcrypt.GenerateFromPassword([]byte("Admin123!"), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		seedUsers := []model.User{
			{Username: "admin", DisplayName: "系统管理员", PasswordHash: string(password), Role: model.RoleAdmin, Active: true},
			{Username: "reviewer", DisplayName: "质量复核员", PasswordHash: string(password), Role: model.RoleReviewer, Active: true},
			{Username: "operator", DisplayName: "现场操作员", PasswordHash: string(password), Role: model.RoleOperator, Active: true},
			{Username: "viewer", DisplayName: "只读审计员", PasswordHash: string(password), Role: model.RoleViewer, Active: true},
		}
		if err := db.WithContext(ctx).Create(&seedUsers).Error; err != nil {
			return err
		}
	}

	if err := seedAircraftPart(ctx, db); err != nil {
		return err
	}

	if err := seedInspectionTask(ctx, db); err != nil {
		return err
	}

	if err := seedCertificateRecord(ctx, db); err != nil {
		return err
	}

	if err := seedReleaseAuthorization(ctx, db); err != nil {
		return err
	}

	return nil
}

func seedAircraftPart(ctx context.Context, db *gorm.DB) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.AircraftPart{}).Count(&count).Error; err != nil || count > 0 {
		return err
	}
	now := time.Now().UTC()
	items := []model.AircraftPart{

		{BaseModel: model.BaseModel{Code: "AP-001", Name: "航空部件示例一", Status: "received", Version: 1,
			Description: "用于启动验证和主要流程演示的航空部件记录"}, Facility: "航空部件适航放行区域1", Owner: "运行一组",
			Category: "常规", RiskLevel: "low", MetricValue: 12.5, MetricUnit: "unit",
			EffectiveAt: now.Add(0 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-01"},

		{BaseModel: model.BaseModel{Code: "AP-002", Name: "航空部件示例二", Status: "inspection", Version: 1,
			Description: "用于启动验证和主要流程演示的航空部件记录"}, Facility: "航空部件适航放行区域2", Owner: "质量复核组",
			Category: "重点", RiskLevel: "medium", MetricValue: 25.0, MetricUnit: "%",
			EffectiveAt: now.Add(3 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-02"},

		{BaseModel: model.BaseModel{Code: "AP-003", Name: "航空部件示例三", Status: "hold", Version: 1,
			Description: "用于启动验证和主要流程演示的航空部件记录"}, Facility: "航空部件适航放行区域3", Owner: "安全主管组",
			Category: "复核", RiskLevel: "high", MetricValue: 37.5, MetricUnit: "score",
			EffectiveAt: now.Add(6 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-03"},

		{BaseModel: model.BaseModel{Code: "AP-004", Name: "航空部件示例四（适航阻断）", Status: "hold", Version: 2,
			Description: "检查任务 IT-004 判定失败后自动转暂停的同编号部件"}, Facility: "航空部件适航放行区域4", Owner: "待复核组",
			Category: "重点", RiskLevel: "critical", MetricValue: 50.0, MetricUnit: "score",
			EffectiveAt: now.Add(9 * time.Hour), Evidence: "检查发现超差，暂停放行", RelatedCode: "REL-515-04",
			BlockingTaskCode: "IT-004", BlockingReason: "叶根裂纹超出允许极限，判定失败待复核", BlockActive: true},
	}
	return db.WithContext(ctx).Create(&items).Error
}

func seedInspectionTask(ctx context.Context, db *gorm.DB) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.InspectionTask{}).Count(&count).Error; err != nil || count > 0 {
		return err
	}
	now := time.Now().UTC()
	items := []model.InspectionTask{

		{BaseModel: model.BaseModel{Code: "IT-001", Name: "检查任务示例一", Status: "planned", Version: 1,
			Description: "用于启动验证和主要流程演示的检查任务记录"}, Facility: "航空部件适航放行区域1", Owner: "运行一组",
			Category: "常规", RiskLevel: "low", MetricValue: 12.5, MetricUnit: "unit",
			EffectiveAt: now.Add(0 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-01"},

		{BaseModel: model.BaseModel{Code: "IT-002", Name: "检查任务示例二", Status: "running", Version: 1,
			Description: "用于启动验证和主要流程演示的检查任务记录"}, Facility: "航空部件适航放行区域2", Owner: "质量复核组",
			Category: "重点", RiskLevel: "medium", MetricValue: 25.0, MetricUnit: "%",
			EffectiveAt: now.Add(3 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-02"},

		{BaseModel: model.BaseModel{Code: "IT-003", Name: "检查任务示例三", Status: "passed", Version: 1,
			Description: "用于启动验证和主要流程演示的检查任务记录"}, Facility: "航空部件适航放行区域3", Owner: "安全主管组",
			Category: "复核", RiskLevel: "high", MetricValue: 37.5, MetricUnit: "score",
			EffectiveAt: now.Add(6 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-03"},

		{BaseModel: model.BaseModel{Code: "IT-004", Name: "检查任务示例四（判定失败）", Status: "failed", Version: 2,
			Description: "判定失败后触发适航阻断闭环的检查任务"}, Facility: "航空部件适航放行区域4", Owner: "待复核组",
			Category: "重点", RiskLevel: "critical", MetricValue: 50.0, MetricUnit: "score",
			EffectiveAt: now.Add(9 * time.Hour), Evidence: "叶根裂纹超出允许极限", RelatedCode: "REL-515-04",
			FailureReason: "叶根裂纹超出允许极限，判定失败待复核"},
	}
	return db.WithContext(ctx).Create(&items).Error
}

func seedCertificateRecord(ctx context.Context, db *gorm.DB) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.CertificateRecord{}).Count(&count).Error; err != nil || count > 0 {
		return err
	}
	now := time.Now().UTC()
	items := []model.CertificateRecord{

		{BaseModel: model.BaseModel{Code: "CR-001", Name: "证书记录示例一", Status: "draft", Version: 1,
			Description: "用于启动验证和主要流程演示的证书记录记录"}, Facility: "航空部件适航放行区域1", Owner: "运行一组",
			Category: "常规", RiskLevel: "low", MetricValue: 12.5, MetricUnit: "unit",
			EffectiveAt: now.Add(0 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-01", PreparedBy: "operator"},

		{BaseModel: model.BaseModel{Code: "CR-002", Name: "证书记录示例二", Status: "valid", Version: 1,
			Description: "用于启动验证和主要流程演示的证书记录记录"}, Facility: "航空部件适航放行区域2", Owner: "质量复核组",
			Category: "重点", RiskLevel: "medium", MetricValue: 25.0, MetricUnit: "%",
			EffectiveAt: now.Add(3 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-02", PreparedBy: "operator", VerifiedBy: "reviewer"},

		{BaseModel: model.BaseModel{Code: "CR-003", Name: "证书记录示例三", Status: "expired", Version: 1,
			Description: "用于启动验证和主要流程演示的证书记录记录"}, Facility: "航空部件适航放行区域3", Owner: "安全主管组",
			Category: "复核", RiskLevel: "high", MetricValue: 37.5, MetricUnit: "score",
			EffectiveAt: now.Add(6 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-03", PreparedBy: "operator", VerifiedBy: "reviewer"},
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&items).Error; err != nil {
			return err
		}
		revisions := make([]model.CertificateRecordRevision, 0, len(items))
		for _, item := range items {
			revisions = append(revisions, model.CertificateRecordRevision{
				CertificateRecordID: item.ID, Version: item.Version, Status: item.Status,
				Evidence: item.Evidence, Actor: "system-seed", RequestID: "seed-gb-515",
				Action: "seed", Reason: "initial demonstration certificate", CreatedAt: now,
			})
		}
		return tx.Create(&revisions).Error
	})
}

func seedReleaseAuthorization(ctx context.Context, db *gorm.DB) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.ReleaseAuthorization{}).Count(&count).Error; err != nil || count > 0 {
		return err
	}
	now := time.Now().UTC()
	items := []model.ReleaseAuthorization{

		{BaseModel: model.BaseModel{Code: "RA-001", Name: "放行授权示例一", Status: "draft", Version: 1,
			Description: "用于启动验证和主要流程演示的放行授权记录"}, Facility: "航空部件适航放行区域1", Owner: "运行一组",
			Category: "常规", RiskLevel: "low", MetricValue: 12.5, MetricUnit: "unit",
			EffectiveAt: now.Add(0 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-01"},

		{BaseModel: model.BaseModel{Code: "RA-002", Name: "放行授权示例二", Status: "review", Version: 1,
			Description: "用于启动验证和主要流程演示的放行授权记录"}, Facility: "航空部件适航放行区域2", Owner: "质量复核组",
			Category: "重点", RiskLevel: "medium", MetricValue: 25.0, MetricUnit: "%",
			EffectiveAt: now.Add(3 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-02", SubmittedBy: "operator"},

		{BaseModel: model.BaseModel{Code: "RA-003", Name: "放行授权示例三", Status: "approved", Version: 1,
			Description: "用于启动验证和主要流程演示的放行授权记录"}, Facility: "航空部件适航放行区域3", Owner: "安全主管组",
			Category: "复核", RiskLevel: "high", MetricValue: 37.5, MetricUnit: "score",
			EffectiveAt: now.Add(6 * time.Hour), Evidence: "已完成基础证据核对", RelatedCode: "REL-515-03", SubmittedBy: "operator", ReviewedBy: "reviewer", ReviewReason: "演示数据双人复核通过"},

		{BaseModel: model.BaseModel{Code: "RA-004", Name: "放行授权示例四（失败转限制）", Status: "restricted", Version: 2,
			Description: "检查任务 IT-004 判定失败后由已批准自动转限制放行，重新检查通过后只能重新提交复核"}, Facility: "航空部件适航放行区域4", Owner: "待复核组",
			Category: "重点", RiskLevel: "critical", MetricValue: 50.0, MetricUnit: "score",
			EffectiveAt: now.Add(9 * time.Hour), Evidence: "原批准证据包冻结", RelatedCode: "REL-515-04",
			SubmittedBy: "operator", ReviewedBy: "reviewer", BlockingTaskCode: "IT-004",
			BlockingReason: "叶根裂纹超出允许极限，判定失败待复核", BlockActive: true},
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&items).Error; err != nil {
			return err
		}
		revisions := make([]model.ReleaseAuthorizationRevision, 0, len(items)+1)
		for _, item := range items {
			revisions = append(revisions, model.ReleaseAuthorizationRevision{
				ReleaseAuthorizationID: item.ID, Version: item.Version, Status: item.Status,
				Evidence: item.Evidence, Actor: "system-seed", RequestID: "seed-gb-515",
				Action: "seed", Reason: "initial demonstration authorization", CreatedAt: now,
			})
		}
		revisions = append(revisions, model.ReleaseAuthorizationRevision{
			ReleaseAuthorizationID: items[3].ID, Version: 1, Status: "approved",
			Evidence: items[3].Evidence, Actor: "reviewer", RequestID: "seed-gb-515",
			Action: "seed", Reason: "initial demonstration approval before inspection failure", CreatedAt: now.Add(-time.Hour),
		})
		return tx.Create(&revisions).Error
	})
}
