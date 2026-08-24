package routing

import (
	"errors"
	"reflect"
	"testing"
)

func TestEngineDecideFiltersBeforeApplyingStaticPolicy(t *testing.T) {
	t.Parallel()

	config := Config{
		Policy: Policy{
			Version:       "policy_v1",
			ProviderOrder: []string{"psp_b", "psp_c", "psp_a"},
		},
		Candidates: []Candidate{
			{
				Provider:                "psp_c",
				Enabled:                 true,
				SupportedCurrencies:     []string{"EUR"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
			eligibleCandidate("psp_a"),
			eligibleCandidate("psp_b"),
			{
				Provider:                "psp_unlisted",
				Enabled:                 true,
				SupportedCurrencies:     []string{"USD"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
		},
	}
	engine := mustEngine(t, config)

	decision, err := engine.Decide(validRequest())
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}

	if decision.SelectedProvider != "psp_b" {
		t.Fatalf("SelectedProvider = %q, want psp_b", decision.SelectedProvider)
	}
	if want := []string{"psp_b", "psp_a"}; !reflect.DeepEqual(decision.CandidateOrder, want) {
		t.Fatalf("CandidateOrder = %#v, want %#v", decision.CandidateOrder, want)
	}
	if decision.PolicyVersion != "policy_v1" {
		t.Fatalf("PolicyVersion = %q, want policy_v1", decision.PolicyVersion)
	}
	if decision.ID == "" {
		t.Fatal("Decision ID is empty")
	}

	wantExclusions := []Exclusion{
		{
			Provider: "psp_c",
			Reasons: []Reason{{
				Code:   ReasonCurrencyNotSupported,
				Detail: "provider does not support currency USD",
			}},
		},
		{
			Provider: "psp_unlisted",
			Reasons: []Reason{{
				Code:   ReasonNotInPolicy,
				Detail: "provider is not present in the active static policy",
			}},
		},
	}
	if !reflect.DeepEqual(decision.Exclusions, wantExclusions) {
		t.Fatalf("Exclusions = %#v, want %#v", decision.Exclusions, wantExclusions)
	}
}

func TestEngineDecisionIsIndependentOfConfiguredCandidateOrder(t *testing.T) {
	t.Parallel()

	firstConfig := validConfig()
	firstConfig.Policy.ProviderOrder = []string{"psp_b", "psp_a"}
	firstConfig.Candidates = []Candidate{eligibleCandidate("psp_a"), eligibleCandidate("psp_b")}

	secondConfig := firstConfig
	secondConfig.Candidates = []Candidate{eligibleCandidate("psp_b"), eligibleCandidate("psp_a")}

	firstDecision, err := mustEngine(t, firstConfig).Decide(validRequest())
	if err != nil {
		t.Fatalf("first Decide() error = %v", err)
	}
	secondDecision, err := mustEngine(t, secondConfig).Decide(validRequest())
	if err != nil {
		t.Fatalf("second Decide() error = %v", err)
	}

	if !reflect.DeepEqual(firstDecision, secondDecision) {
		t.Fatalf("decisions differ by configured candidate order:\nfirst:  %#v\nsecond: %#v", firstDecision, secondDecision)
	}
}

func TestEngineOwnsImmutableConfiguration(t *testing.T) {
	t.Parallel()

	config := validConfig()
	engine := mustEngine(t, config)
	config.Policy.ProviderOrder[0] = "mutated"
	config.Candidates[0].Enabled = false
	config.Candidates[0].SupportedCurrencies[0] = "JPY"

	decision, err := engine.Decide(validRequest())
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if decision.SelectedProvider != "psp_a" {
		t.Fatalf("SelectedProvider = %q, want psp_a", decision.SelectedProvider)
	}
}

func TestEngineNoEligibleProviderReturnsDecisionEvidence(t *testing.T) {
	t.Parallel()

	config := validConfig()
	config.Candidates[0].Enabled = false
	engine := mustEngine(t, config)

	decision, err := engine.Decide(validRequest())
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("Decide() error = %v, want ErrNoEligibleProvider", err)
	}
	if decision.ID == "" {
		t.Fatal("Decision ID is empty")
	}
	if len(decision.CandidateOrder) != 0 {
		t.Fatalf("CandidateOrder = %#v, want empty", decision.CandidateOrder)
	}
	if len(decision.Exclusions) != 1 || decision.Exclusions[0].Reasons[0].Code != ReasonProviderDisabled {
		t.Fatalf("Exclusions = %#v, want provider_disabled", decision.Exclusions)
	}
}

func TestEngineRejectsDuplicateConfiguredProvider(t *testing.T) {
	t.Parallel()

	config := validConfig()
	config.Candidates = append(config.Candidates, eligibleCandidate("psp_a"))

	_, err := NewEngine(config)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("NewEngine() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "candidates[1].provider" {
		t.Fatalf("ValidationError.Field = %q, want candidates[1].provider", validationErr.Field)
	}
}

func TestEngineRejectsTokenInPaymentMethodCategory(t *testing.T) {
	t.Parallel()

	request := validRequest()
	request.PaymentMethod = "pm_secret_123"

	_, err := mustEngine(t, validConfig()).Decide(request)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Decide() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "payment_method" {
		t.Fatalf("ValidationError.Field = %q, want payment_method", validationErr.Field)
	}
}

func TestEngineSupportsAuthorizeForFutureManualCapture(t *testing.T) {
	t.Parallel()

	request := validRequest()
	request.Operation = "authorize"
	decision, err := mustEngine(t, validConfig()).Decide(request)
	if err != nil {
		t.Fatalf("Decide(authorize) error = %v", err)
	}
	if decision.SelectedProvider != "psp_a" {
		t.Fatalf("SelectedProvider = %q, want psp_a", decision.SelectedProvider)
	}
}

func validRequest() Request {
	return Request{
		PaymentID:     "pay_123",
		AttemptID:     "att_123",
		MerchantID:    "merchant_123",
		AmountMinor:   2599,
		Currency:      "USD",
		PaymentMethod: "card",
		Operation:     "sale",
	}
}

func validConfig() Config {
	return Config{
		Policy: Policy{
			Version:       "policy_v1",
			ProviderOrder: []string{"psp_a"},
		},
		Candidates: []Candidate{eligibleCandidate("psp_a")},
	}
}

func eligibleCandidate(provider string) Candidate {
	return Candidate{
		Provider:                provider,
		Enabled:                 true,
		SupportedCurrencies:     []string{"USD", "EUR"},
		SupportedPaymentMethods: []string{"card"},
		SupportedOperations:     []string{"sale", "authorize"},
	}
}

func mustEngine(t *testing.T, config Config) *Engine {
	t.Helper()
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	return engine
}
