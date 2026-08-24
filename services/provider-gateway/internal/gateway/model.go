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

type OperationRequest struct {
	OperationID        string `json:"operation_id"`
	AttemptID          string `json:"attempt_id"`
	Provider           string `json:"provider"`
	AmountMinor        int64  `json:"amount_minor"`
	Currency           string `json:"currency"`
	PaymentMethodToken string `json:"payment_method_token"`
	CaptureMethod      string `json:"capture_method"`
	Scenario           string `json:"scenario,omitempty"`
}

type Operation struct {
	OperationID            string    `json:"operation_id"`
	AttemptID              string    `json:"attempt_id"`
	Provider               string    `json:"provider"`
	ProviderIdempotencyKey string    `json:"provider_idempotency_key"`
	RequestHash            string    `json:"-"`
	Status                 string    `json:"status"`
	SideEffectCertainty    string    `json:"side_effect_certainty"`
	ProviderReference      string    `json:"provider_reference,omitempty"`
	FailureCode            string    `json:"failure_code,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type ProviderOutcome struct {
	EventID             string    `json:"event_id"`
	OperationID         string    `json:"operation_id"`
	Provider            string    `json:"provider"`
	Status              string    `json:"status"`
	SideEffectCertainty string    `json:"side_effect_certainty"`
	ProviderReference   string    `json:"provider_reference,omitempty"`
	FailureCode         string    `json:"failure_code,omitempty"`
	OccurredAt          time.Time `json:"occurred_at"`
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
