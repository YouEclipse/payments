package routing

// Request is the complete, non-sensitive snapshot used to make one routing
// decision. Payment method credentials deliberately do not belong here.
type Request struct {
	PaymentID     string
	AttemptID     string
	MerchantID    string
	AmountMinor   int64
	Currency      string
	PaymentMethod string
	Operation     string
}

// Config is the Router-owned capability and policy snapshot. It is injected
// when the Engine is constructed and never accepted from Payment API callers.
type Config struct {
	Policy     Policy      `json:"policy"`
	Candidates []Candidate `json:"candidates"`
}

// Policy describes the versioned, deterministic provider order used by the
// first routing implementation.
type Policy struct {
	Version       string   `json:"version"`
	ProviderOrder []string `json:"provider_order"`
}

// Candidate is a provider capability snapshot. Enabled and all supported
// capabilities are hard eligibility constraints, not scoring inputs.
type Candidate struct {
	Provider                string   `json:"provider"`
	Enabled                 bool     `json:"enabled"`
	SupportedCurrencies     []string `json:"supported_currencies"`
	SupportedPaymentMethods []string `json:"supported_payment_methods"`
	SupportedOperations     []string `json:"supported_operations"`
}

// Decision records both the selected route and the evidence needed to explain
// it. CandidateOrder contains only eligible providers, in policy order.
type Decision struct {
	ID               string      `json:"decision_id"`
	SelectedProvider string      `json:"selected_provider,omitempty"`
	CandidateOrder   []string    `json:"candidate_order"`
	Exclusions       []Exclusion `json:"exclusions"`
	PolicyVersion    string      `json:"policy_version"`
}

// Exclusion explains why a provider could not be selected.
type Exclusion struct {
	Provider string   `json:"provider"`
	Reasons  []Reason `json:"reasons"`
}

// Reason has a stable machine-readable Code and a human-readable Detail.
type Reason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

const (
	ReasonProviderDisabled          = "provider_disabled"
	ReasonCurrencyNotSupported      = "currency_not_supported"
	ReasonPaymentMethodNotSupported = "payment_method_not_supported"
	ReasonOperationNotSupported     = "operation_not_supported"
	ReasonNotInPolicy               = "not_in_policy"
)
