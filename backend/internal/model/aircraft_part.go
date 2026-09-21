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

	// Airworthiness blocking stamp produced by a failed inspection task sharing
	// the same RelatedCode. BlockActive is cleared once every failed task for the
	// code passes re-inspection; BlockingTaskCode/BlockingReason are retained so
	// the page can still explain why the part was put on hold.
	BlockingTaskCode string `json:"blockingTaskCode" gorm:"size:64;index"`
	BlockingReason   string `json:"blockingReason" gorm:"size:500"`
	BlockActive      bool   `json:"blockActive" gorm:"not null;default:false;index"`
}

func (item *AircraftPart) GetBase() *BaseModel { return &item.BaseModel }

func (item AircraftPart) TableName() string { return "aircraft_parts" }

var AircraftPartInitialStatus = "received"
