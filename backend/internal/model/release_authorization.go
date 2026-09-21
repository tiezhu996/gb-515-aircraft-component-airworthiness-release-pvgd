package model

import "time"

// ReleaseAuthorization models 放行授权 as an independently versioned aggregate. The fields
// cover ownership, operational context, evidence and measured risk so later
// changes naturally span persistence, service and UI layers.
type ReleaseAuthorization struct {
	BaseModel
	Facility     string                         `json:"facility" gorm:"size:120;index"`
	Owner        string                         `json:"owner" gorm:"size:120;index"`
	Category     string                         `json:"category" gorm:"size:80;index"`
	RiskLevel    string                         `json:"riskLevel" gorm:"size:32;index"`
	MetricValue  float64                        `json:"metricValue"`
	MetricUnit   string                         `json:"metricUnit" gorm:"size:24"`
	EffectiveAt  time.Time                      `json:"effectiveAt"`
	Evidence     string                         `json:"evidence" gorm:"size:2000"`
	RelatedCode  string                         `json:"relatedCode" gorm:"size:64;index"`
	SubmittedBy  string                         `json:"submittedBy" gorm:"size:80;index"`
	ReviewedBy   string                         `json:"reviewedBy" gorm:"size:80;index"`
	ReviewReason string                         `json:"reviewReason" gorm:"size:500"`
	Revisions    []ReleaseAuthorizationRevision `json:"revisions,omitempty" gorm:"foreignKey:ReleaseAuthorizationID"`

	// Airworthiness blocking chain. A failed inspection task for the same
	// RelatedCode forces review/approved authorizations to restricted and
	// records the failed task code and reason. After the re-inspection passes
	// the block is resolved exactly once and the authorization must be
	// resubmitted for the standard two-person review; it cannot return to
	// approved directly.
	BlockedByTaskCode string     `json:"blockedByTaskCode" gorm:"size:64;index"`
	BlockReason       string     `json:"blockReason" gorm:"size:500"`
	BlockedAt         *time.Time `json:"blockedAt"`
	BlockResolvedAt   *time.Time `json:"blockResolvedAt"`
}

func (item *ReleaseAuthorization) GetBase() *BaseModel { return &item.BaseModel }

func (item ReleaseAuthorization) TableName() string { return "release_authorizations" }

var ReleaseAuthorizationInitialStatus = "draft"

// Blocked reports whether an inspection-failure airworthiness block is
// currently active on the authorization.
func (item *ReleaseAuthorization) Blocked() bool {
	return item.BlockedByTaskCode != "" && item.BlockedAt != nil && item.BlockResolvedAt == nil
}

// ReleaseAuthorizationRevision preserves the complete authorization decision
// chain, including who acted and which request produced the version.
type ReleaseAuthorizationRevision struct {
	ID                     uint      `json:"id" gorm:"primaryKey"`
	ReleaseAuthorizationID uint      `json:"releaseAuthorizationId" gorm:"not null;index;uniqueIndex:idx_authorization_revision_version,priority:1"`
	Version                uint      `json:"version" gorm:"not null;uniqueIndex:idx_authorization_revision_version,priority:2"`
	Status                 string    `json:"status" gorm:"size:40;not null"`
	Evidence               string    `json:"evidence" gorm:"size:2000"`
	Actor                  string    `json:"actor" gorm:"size:80;not null;index"`
	RequestID              string    `json:"requestId" gorm:"size:64;not null;index"`
	Action                 string    `json:"action" gorm:"size:40;not null"`
	Reason                 string    `json:"reason" gorm:"size:500"`
	CreatedAt              time.Time `json:"createdAt" gorm:"index"`
}
