package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrNoEligibleProvider = errors.New("no eligible provider")

// ValidationError identifies a malformed or internally inconsistent routing
// request. Its message is safe to return to another internal service.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Engine evaluates deterministic hard constraints before applying its own
// static, versioned provider policy. Configuration is immutable after startup.
type Engine struct {
	policy     Policy
	candidates []Candidate
}

func NewEngine(config Config) (*Engine, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	config = cloneConfig(config)
	return &Engine{
		policy:     config.Policy,
		candidates: config.Candidates,
	}, nil
}

// Decide returns a Decision even when ErrNoEligibleProvider is returned so the
// caller can retain the complete exclusion evidence.
func (e *Engine) Decide(req Request) (Decision, error) {
	if err := validateRequest(req); err != nil {
		return Decision{}, err
	}

	policyPosition := make(map[string]struct{}, len(e.policy.ProviderOrder))
	for _, provider := range e.policy.ProviderOrder {
		policyPosition[provider] = struct{}{}
	}

	eligible := make(map[string]bool, len(e.candidates))
	exclusions := make([]Exclusion, 0, len(e.candidates))

	for _, candidate := range e.candidates {
		reasons := eligibilityReasons(req, candidate)
		if _, ok := policyPosition[candidate.Provider]; !ok {
			reasons = append(reasons, Reason{
				Code:   ReasonNotInPolicy,
				Detail: "provider is not present in the active static policy",
			})
		}

		if len(reasons) == 0 {
			eligible[candidate.Provider] = true
			continue
		}

		exclusions = append(exclusions, Exclusion{
			Provider: candidate.Provider,
			Reasons:  reasons,
		})
	}

	sort.Slice(exclusions, func(i, j int) bool {
		return exclusions[i].Provider < exclusions[j].Provider
	})

	candidateOrder := make([]string, 0, len(eligible))
	for _, provider := range e.policy.ProviderOrder {
		if eligible[provider] {
			candidateOrder = append(candidateOrder, provider)
		}
	}

	decision := Decision{
		CandidateOrder: candidateOrder,
		Exclusions:     exclusions,
		PolicyVersion:  e.policy.Version,
	}
	if len(candidateOrder) > 0 {
		decision.SelectedProvider = candidateOrder[0]
	}
	decision.ID = decisionID(req, e.policy, e.candidates, decision)

	if len(candidateOrder) == 0 {
		return decision, ErrNoEligibleProvider
	}
	return decision, nil
}

func eligibilityReasons(req Request, candidate Candidate) []Reason {
	reasons := make([]Reason, 0, 4)
	if !candidate.Enabled {
		reasons = append(reasons, Reason{
			Code:   ReasonProviderDisabled,
			Detail: "provider is disabled",
		})
	}
	if !contains(candidate.SupportedCurrencies, req.Currency) {
		reasons = append(reasons, Reason{
			Code:   ReasonCurrencyNotSupported,
			Detail: fmt.Sprintf("provider does not support currency %s", req.Currency),
		})
	}
	if !contains(candidate.SupportedPaymentMethods, req.PaymentMethod) {
		reasons = append(reasons, Reason{
			Code:   ReasonPaymentMethodNotSupported,
			Detail: fmt.Sprintf("provider does not support payment method %s", req.PaymentMethod),
		})
	}
	if !contains(candidate.SupportedOperations, req.Operation) {
		reasons = append(reasons, Reason{
			Code:   ReasonOperationNotSupported,
			Detail: fmt.Sprintf("provider does not support operation %s", req.Operation),
		})
	}
	return reasons
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validateRequest(req Request) error {
	required := []struct {
		field string
		value string
	}{
		{field: "payment_id", value: req.PaymentID},
		{field: "attempt_id", value: req.AttemptID},
		{field: "merchant_id", value: req.MerchantID},
		{field: "currency", value: req.Currency},
		{field: "payment_method", value: req.PaymentMethod},
		{field: "operation", value: req.Operation},
	}
	for _, item := range required {
		if strings.TrimSpace(item.value) == "" {
			return &ValidationError{Field: item.field, Message: "must not be empty"}
		}
	}

	if req.AmountMinor <= 0 {
		return &ValidationError{Field: "amount_minor", Message: "must be greater than zero"}
	}
	if !isCurrencyCode(req.Currency) {
		return &ValidationError{Field: "currency", Message: "must be a three-letter uppercase currency code"}
	}
	if req.PaymentMethod != "card" {
		return &ValidationError{Field: "payment_method", Message: "only the non-sensitive card category is supported in this version"}
	}
	if !isSupportedOperation(req.Operation) {
		return &ValidationError{Field: "operation", Message: "must be sale or authorize"}
	}

	return nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.Policy.Version) == "" {
		return &ValidationError{Field: "policy.version", Message: "must not be empty"}
	}
	if len(config.Candidates) == 0 {
		return &ValidationError{Field: "candidates", Message: "must contain at least one candidate"}
	}
	if len(config.Policy.ProviderOrder) == 0 {
		return &ValidationError{Field: "policy.provider_order", Message: "must contain at least one provider"}
	}

	providers := make(map[string]struct{}, len(config.Candidates))
	for i, candidate := range config.Candidates {
		field := fmt.Sprintf("candidates[%d].provider", i)
		if strings.TrimSpace(candidate.Provider) == "" {
			return &ValidationError{Field: field, Message: "must not be empty"}
		}
		if _, exists := providers[candidate.Provider]; exists {
			return &ValidationError{Field: field, Message: "must be unique"}
		}
		providers[candidate.Provider] = struct{}{}

		if err := validateCapabilities(i, candidate); err != nil {
			return err
		}
	}

	policyProviders := make(map[string]struct{}, len(config.Policy.ProviderOrder))
	for i, provider := range config.Policy.ProviderOrder {
		field := fmt.Sprintf("policy.provider_order[%d]", i)
		if strings.TrimSpace(provider) == "" {
			return &ValidationError{Field: field, Message: "must not be empty"}
		}
		if _, exists := policyProviders[provider]; exists {
			return &ValidationError{Field: field, Message: "must be unique"}
		}
		if _, exists := providers[provider]; !exists {
			return &ValidationError{Field: field, Message: "must reference a candidate"}
		}
		policyProviders[provider] = struct{}{}
	}

	return nil
}

func validateCapabilities(candidateIndex int, candidate Candidate) error {
	seenCurrencies := make(map[string]struct{}, len(candidate.SupportedCurrencies))
	for i, currency := range candidate.SupportedCurrencies {
		field := fmt.Sprintf("candidates[%d].supported_currencies[%d]", candidateIndex, i)
		if !isCurrencyCode(currency) {
			return &ValidationError{Field: field, Message: "must be a three-letter uppercase currency code"}
		}
		if _, exists := seenCurrencies[currency]; exists {
			return &ValidationError{Field: field, Message: "must be unique"}
		}
		seenCurrencies[currency] = struct{}{}
	}

	seenPaymentMethods := make(map[string]struct{}, len(candidate.SupportedPaymentMethods))
	for i, paymentMethod := range candidate.SupportedPaymentMethods {
		field := fmt.Sprintf("candidates[%d].supported_payment_methods[%d]", candidateIndex, i)
		if paymentMethod != "card" {
			return &ValidationError{Field: field, Message: "only the non-sensitive card category is supported in this version"}
		}
		if _, exists := seenPaymentMethods[paymentMethod]; exists {
			return &ValidationError{Field: field, Message: "must be unique"}
		}
		seenPaymentMethods[paymentMethod] = struct{}{}
	}

	seenOperations := make(map[string]struct{}, len(candidate.SupportedOperations))
	for i, operation := range candidate.SupportedOperations {
		field := fmt.Sprintf("candidates[%d].supported_operations[%d]", candidateIndex, i)
		if !isSupportedOperation(operation) {
			return &ValidationError{Field: field, Message: "must be sale or authorize"}
		}
		if _, exists := seenOperations[operation]; exists {
			return &ValidationError{Field: field, Message: "must be unique"}
		}
		seenOperations[operation] = struct{}{}
	}

	return nil
}

func isCurrencyCode(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func isSupportedOperation(value string) bool {
	return value == "sale" || value == "authorize"
}

func cloneConfig(config Config) Config {
	cloned := Config{
		Policy: Policy{
			Version:       config.Policy.Version,
			ProviderOrder: append([]string(nil), config.Policy.ProviderOrder...),
		},
		Candidates: make([]Candidate, 0, len(config.Candidates)),
	}
	for _, candidate := range config.Candidates {
		cloned.Candidates = append(cloned.Candidates, Candidate{
			Provider:                candidate.Provider,
			Enabled:                 candidate.Enabled,
			SupportedCurrencies:     append([]string(nil), candidate.SupportedCurrencies...),
			SupportedPaymentMethods: append([]string(nil), candidate.SupportedPaymentMethods...),
			SupportedOperations:     append([]string(nil), candidate.SupportedOperations...),
		})
	}
	return cloned
}

func decisionID(req Request, policy Policy, configuredCandidates []Candidate, decision Decision) string {
	type canonicalCandidate struct {
		Provider                string   `json:"provider"`
		Enabled                 bool     `json:"enabled"`
		SupportedCurrencies     []string `json:"supported_currencies"`
		SupportedPaymentMethods []string `json:"supported_payment_methods"`
		SupportedOperations     []string `json:"supported_operations"`
	}

	candidates := make([]canonicalCandidate, 0, len(configuredCandidates))
	for _, candidate := range configuredCandidates {
		currencies := append([]string(nil), candidate.SupportedCurrencies...)
		paymentMethods := append([]string(nil), candidate.SupportedPaymentMethods...)
		operations := append([]string(nil), candidate.SupportedOperations...)
		sort.Strings(currencies)
		sort.Strings(paymentMethods)
		sort.Strings(operations)
		candidates = append(candidates, canonicalCandidate{
			Provider:                candidate.Provider,
			Enabled:                 candidate.Enabled,
			SupportedCurrencies:     currencies,
			SupportedPaymentMethods: paymentMethods,
			SupportedOperations:     operations,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Provider < candidates[j].Provider
	})

	canonical := struct {
		PaymentID           string               `json:"payment_id"`
		AttemptID           string               `json:"attempt_id"`
		MerchantID          string               `json:"merchant_id"`
		AmountMinor         int64                `json:"amount_minor"`
		Currency            string               `json:"currency"`
		PaymentMethod       string               `json:"payment_method"`
		Operation           string               `json:"operation"`
		PolicyVersion       string               `json:"policy_version"`
		PolicyProviderOrder []string             `json:"policy_provider_order"`
		Candidates          []canonicalCandidate `json:"candidates"`
		CandidateOrder      []string             `json:"candidate_order"`
		Exclusions          []Exclusion          `json:"exclusions"`
	}{
		PaymentID:           req.PaymentID,
		AttemptID:           req.AttemptID,
		MerchantID:          req.MerchantID,
		AmountMinor:         req.AmountMinor,
		Currency:            req.Currency,
		PaymentMethod:       req.PaymentMethod,
		Operation:           req.Operation,
		PolicyVersion:       decision.PolicyVersion,
		PolicyProviderOrder: append([]string(nil), policy.ProviderOrder...),
		Candidates:          candidates,
		CandidateOrder:      decision.CandidateOrder,
		Exclusions:          decision.Exclusions,
	}

	payload, err := json.Marshal(canonical)
	if err != nil {
		panic("routing decision canonical form cannot fail to marshal: " + err.Error())
	}
	digest := sha256.Sum256(payload)
	return "rtd_" + hex.EncodeToString(digest[:12])
}
