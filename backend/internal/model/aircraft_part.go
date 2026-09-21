package model

import "time"

// AircraftPart models 航空部件 as an independently versioned aggregate. The fields
// cover ownership, operational context, evidence and measured risk so later
// changes naturally span persistence, service and UI layers.
type AircraftPart struct {
	BaseModel
	Facility    string    `json:"facility" gorm:"size:120;index"`
	Owner       string    `json:"owner" gorm:"size:120;index"`
	Category    string    `json:"category" gorm:"size:80;index"`
	RiskLevel   string    `json:"riskLevel" gorm:"size:32;index"`
	MetricValue float64   `json:"metricValue"`
	MetricUnit  string    `json:"metricUnit" gorm:"size:24"`
	EffectiveAt time.Time `json:"effectiveAt"`
	Evidence    string    `json:"evidence" gorm:"size:2000"`
	RelatedCode string    `json:"relatedCode" gorm:"size:64;index"`

	// Airworthiness blocking chain. When an inspection task with the same
	// RelatedCode fails, the part is moved to hold and the blocking origin is
	// recorded so every page can show the failed task code and reason after a
	// refresh. PreBlockStatus remembers where the part was paused so the
	// one-shot recovery can resume it when the re-inspection passes.
	PreBlockStatus    string     `json:"preBlockStatus" gorm:"size:40"`
	BlockedByTaskCode string     `json:"blockedByTaskCode" gorm:"size:64;index"`
	BlockReason       string     `json:"blockReason" gorm:"size:500"`
	BlockedAt         *time.Time `json:"blockedAt"`
	BlockResolvedAt   *time.Time `json:"blockResolvedAt"`
}

func (item *AircraftPart) GetBase() *BaseModel { return &item.BaseModel }

// Blocked reports whether an inspection-failure airworthiness block is
// currently active on the part.
func (item *AircraftPart) Blocked() bool {
	return item.BlockedByTaskCode != "" && item.BlockedAt != nil && item.BlockResolvedAt == nil
}

func (item AircraftPart) TableName() string { return "aircraft_parts" }

var AircraftPartInitialStatus = "received"
