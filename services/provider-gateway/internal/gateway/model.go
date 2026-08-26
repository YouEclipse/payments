package gateway

import "time"

const (
	StatusCreated   = "created"
	StatusSending   = "sending"
	StatusUnknown   = "unknown"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusReview    = "review_required"
)

const (
	requestHashVersionLegacy     = 1
	requestHashVersionConnection = 2
	legacyConnectionRevision     = "legacy_v1"
	executionFingerprintVersion  = 1
)

// ProviderConnectionIdentity is the immutable execution identity configured
// for one provider connection. ConnectionRevision pins the concrete account,
// credentials, endpoint and connector revision used for retries. NamespaceID
// is a conservative provider-side collision domain shared by recovery aliases;
// sharing it does not grant cross-connection webhook authority.
type ProviderConnectionIdentity struct {
	Provider           string
	ConnectionRevision string
	NamespaceID        string
}

// ProviderConnectionRevisionBinding makes a revision immutable across process
// restarts. The fingerprint contains execution configuration, never raw secrets.
type ProviderConnectionRevisionBinding struct {
	ProviderConnectionIdentity
	FingerprintVersion int
	ConfigFingerprint  string
}

type OperationRequest struct {
	OperationID                string `json:"operation_id"`
	AttemptID                  string `json:"attempt_id"`
	ProviderConnectionID       string `json:"provider_connection_id"`
	ProviderConnectionRevision string `json:"provider_connection_revision"`
	ProviderNamespaceID        string `json:"provider_namespace_id"`
	Provider                   string `json:"provider"`
	AmountMinor                int64  `json:"amount_minor"`
	Currency                   string `json:"currency"`
	PaymentMethodToken         string `json:"payment_method_token"`
	CaptureMethod              string `json:"capture_method"`
	Scenario                   string `json:"scenario,omitempty"`
}

type Operation struct {
	OperationID                string    `json:"operation_id"`
	AttemptID                  string    `json:"attempt_id"`
	ProviderConnectionID       string    `json:"provider_connection_id"`
	ProviderConnectionRevision string    `json:"provider_connection_revision"`
	ProviderNamespaceID        string    `json:"provider_namespace_id"`
	Provider                   string    `json:"provider"`
	ProviderIdempotencyKey     string    `json:"provider_idempotency_key"`
	RequestHash                string    `json:"-"`
	RequestHashVersion         int       `json:"-"`
	LegacyRequestHash          string    `json:"-"`
	Status                     string    `json:"status"`
	SideEffectCertainty        string    `json:"side_effect_certainty"`
	ProviderReference          string    `json:"provider_reference,omitempty"`
	FailureCode                string    `json:"failure_code,omitempty"`
	CreatedAt                  time.Time `json:"created_at"`
	UpdatedAt                  time.Time `json:"updated_at"`
}

type ProviderOutcome struct {
	EventID                    string    `json:"event_id"`
	OperationID                string    `json:"operation_id"`
	ProviderConnectionID       string    `json:"provider_connection_id"`
	ProviderConnectionRevision string    `json:"provider_connection_revision"`
	ProviderNamespaceID        string    `json:"provider_namespace_id"`
	Provider                   string    `json:"provider"`
	Status                     string    `json:"status"`
	SideEffectCertainty        string    `json:"side_effect_certainty"`
	ProviderReference          string    `json:"provider_reference,omitempty"`
	FailureCode                string    `json:"failure_code,omitempty"`
	OccurredAt                 time.Time `json:"occurred_at"`
}

// VerifiedWebhookEvent separates the authenticated callback path identity
// from the normalized provider evidence. The store correlates OperationID and
// replaces the path tuple with the durable operation tuple before persistence.
type VerifiedWebhookEvent struct {
	PathConnectionID           string
	PathIdentity               ProviderConnectionIdentity
	AllowedOperationIdentities map[string]ProviderConnectionIdentity
	EventID                    string
	OperationID                string
	Status                     string
	SideEffectCertainty        string
	ProviderReference          string
	FailureCode                string
	OccurredAt                 time.Time
}

type OutboxItem struct {
	ID      int64
	Outcome ProviderOutcome
}

type providerResponse struct {
	Provider          string `json:"provider"`
	OperationID       string `json:"operation_id"`
	ProviderReference string `json:"provider_reference,omitempty"`
	Status            string `json:"status"`
	FailureCode       string `json:"failure_code,omitempty"`
}
