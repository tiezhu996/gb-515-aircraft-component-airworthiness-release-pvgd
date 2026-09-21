package constants

// Shared status values are mirrored in frontend/src/types/status.ts. Keeping
// the lists explicit makes state-machine drift visible during code review.

type PartState string

const (
	PartStateReceived   PartState = "received"
	PartStateInspection PartState = "inspection"
	PartStateHold       PartState = "hold"
	PartStateReleased   PartState = "released"
	PartStateRetired    PartState = "retired"
)

var AllPartState = []string{"received", "inspection", "hold", "released", "retired"}

type AuthorizationState string

const (
	AuthorizationStateDraft      AuthorizationState = "draft"
	AuthorizationStateReview     AuthorizationState = "review"
	AuthorizationStateApproved   AuthorizationState = "approved"
	AuthorizationStateRestricted AuthorizationState = "restricted"
	AuthorizationStateRevoked    AuthorizationState = "revoked"
)

var AllAuthorizationState = []string{"draft", "review", "approved", "restricted", "revoked"}

var AircraftPartTransitions = map[string]map[string]bool{
	"received":   {"inspection": true, "hold": true},
	"inspection": {"hold": true, "released": true, "received": true},
	"hold":       {"released": true, "retired": true, "inspection": true},
	"released":   {"retired": true, "hold": true},
	"retired":    {"released": true},
}

var InspectionTaskTransitions = map[string]map[string]bool{
	"planned": {"running": true},
	"running": {"passed": true, "failed": true},
	"passed":  {},
	"failed":  {"running": true},
}

var CertificateRecordTransitions = map[string]map[string]bool{
	"draft":   {"valid": true},
	"valid":   {"expired": true, "revoked": true},
	"expired": {"revoked": true},
	"revoked": {},
}

var ReleaseAuthorizationTransitions = map[string]map[string]bool{
	"draft":    {"review": true},
	"review":   {"approved": true, "restricted": true, "draft": true},
	"approved": {"restricted": true, "revoked": true},
	// restricted -> review exists only for authorizations restricted by an
	// inspection-failure block after the re-inspection passes. The service
	// layer enforces that gate; reviewer-initiated restrictions keep the
	// original terminal behaviour.
	"restricted": {"revoked": true, "review": true},
	"revoked":    {},
}

func CanTransition(graph map[string]map[string]bool, from, to string) bool {
	targets, exists := graph[from]
	return exists && targets[to]
}
