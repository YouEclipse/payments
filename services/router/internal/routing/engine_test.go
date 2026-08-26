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
			Version:        "policy_v1",
			CandidateOrder: []string{"psp_b", "psp_c", "psp_a"},
		},
		Candidates: []Candidate{
			{
				ID:                      "psp_c",
				ConnectionRevision:      "psp_c_v1",
				ProviderNamespaceID:     "psp_c",
				Provider:                "psp_c",
				MerchantAccount:         "acct_c",
				Connector:               "psp_c_connector",
				Region:                  "us-east-1",
				Enabled:                 true,
				SupportedCurrencies:     []string{"EUR"},
				SupportedPaymentMethods: []string{"card"},
				SupportedOperations:     []string{"sale", "authorize"},
			},
			eligibleCandidate("psp_a"),
			eligibleCandidate("psp_b"),
			{
				ID:                      "psp_unlisted",
				ConnectionRevision:      "psp_unlisted_v1",
				ProviderNamespaceID:     "psp_unlisted",
				Provider:                "psp_unlisted",
				MerchantAccount:         "acct_unlisted",
				Connector:               "psp_unlisted_connector",
				Region:                  "us-east-1",
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

	if decision.SelectedCandidate.ID != "psp_b" {
		t.Fatalf("SelectedCandidate.ID = %q, want psp_b", decision.SelectedCandidate.ID)
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
			CandidateID: "psp_c",
			Provider:    "psp_c",
			Reasons: []Reason{{
				Code:   ReasonCurrencyNotSupported,
				Detail: "provider does not support currency USD",
			}},
		},
		{
			CandidateID: "psp_unlisted",
			Provider:    "psp_unlisted",
			Reasons: []Reason{{
				Code:   ReasonNotInPolicy,
				Detail: "candidate is not present in the active static policy",
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
	firstConfig.Policy.CandidateOrder = []string{"psp_b", "psp_a"}
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

func TestDecisionDigestIncludesExecutionRevisionAndNamespace(t *testing.T) {
	baseline, err := mustEngine(t, validConfig()).Decide(validRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"revision", "namespace"} {
		t.Run(field, func(t *testing.T) {
			config := validConfig()
			if field == "revision" {
				config.Candidates[0].ConnectionRevision = "psp_a_v2"
			} else {
				config.Candidates[0].ProviderNamespaceID = "psp_a_account_2"
			}
			decision, err := mustEngine(t, config).Decide(validRequest())
			if err != nil {
				t.Fatal(err)
			}
			if decision.ID == baseline.ID {
				t.Fatalf("decision digest ignored %s", field)
			}
			if decision.SelectedCandidate.ConnectionRevision != config.Candidates[0].ConnectionRevision ||
				decision.SelectedCandidate.ProviderNamespaceID != config.Candidates[0].ProviderNamespaceID {
				t.Fatalf("decision lost pinned execution identity: %+v", decision.SelectedCandidate)
			}
		})
	}
}

func TestEngineOwnsImmutableConfiguration(t *testing.T) {
	t.Parallel()

	config := validConfig()
	engine := mustEngine(t, config)
	config.Policy.CandidateOrder[0] = "mutated"
	config.Candidates[0].Enabled = false
	config.Candidates[0].SupportedCurrencies[0] = "JPY"

	decision, err := engine.Decide(validRequest())
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if decision.SelectedCandidate.ID != "psp_a" {
		t.Fatalf("SelectedCandidate.ID = %q, want psp_a", decision.SelectedCandidate.ID)
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

func TestEngineRejectsDuplicateCandidateID(t *testing.T) {
	t.Parallel()

	config := validConfig()
	config.Candidates = append(config.Candidates, eligibleCandidate("psp_a"))

	_, err := NewEngine(config)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("NewEngine() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "candidates[1].candidate_id" {
		t.Fatalf("ValidationError.Field = %q, want candidates[1].candidate_id", validationErr.Field)
	}
}

func TestEngineRejectsNonCanonicalConnectionIdentity(t *testing.T) {
	t.Parallel()

	config := validConfig()
	config.Candidates[0].ID = " psp_a"
	config.Policy.CandidateOrder[0] = " psp_a"

	_, err := NewEngine(config)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("NewEngine() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "candidates[0].candidate_id" {
		t.Fatalf("ValidationError.Field = %q, want candidates[0].candidate_id", validationErr.Field)
	}
}

func TestEngineRejectsAliasesForTheSameExecutionConnection(t *testing.T) {
	t.Parallel()

	config := validConfig()
	alias := config.Candidates[0]
	alias.ID = "psp_a_alias"
	config.Candidates = append(config.Candidates, alias)
	config.Policy.CandidateOrder = append(config.Policy.CandidateOrder, alias.ID)

	_, err := NewEngine(config)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("NewEngine() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "candidates[1].candidate_id" {
		t.Fatalf("ValidationError.Field = %q, want candidates[1].candidate_id", validationErr.Field)
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
	if decision.SelectedCandidate.ID != "psp_a" {
		t.Fatalf("SelectedCandidate.ID = %q, want psp_a", decision.SelectedCandidate.ID)
	}
}

func TestEngineDistinguishesTwoConnectionsForSameProvider(t *testing.T) {
	t.Parallel()

	primary := eligibleCandidate("psp_a_us_primary")
	primary.Provider = "psp_a"
	primary.MerchantAccount = "acct_us"
	backup := eligibleCandidate("psp_a_eu_backup")
	backup.Provider = "psp_a"
	backup.MerchantAccount = "acct_eu"
	backup.Region = "eu-west-1"
	config := Config{
		Policy:     Policy{Version: "policy_v2", CandidateOrder: []string{backup.ID, primary.ID}},
		Candidates: []Candidate{primary, backup},
	}

	decision, err := mustEngine(t, config).Decide(validRequest())
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if decision.SelectedCandidate.ID != backup.ID || decision.SelectedCandidate.Provider != "psp_a" || decision.SelectedCandidate.MerchantAccount != "acct_eu" {
		t.Fatalf("SelectedCandidate = %#v, want exact EU connection", decision.SelectedCandidate)
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
			Version:        "policy_v1",
			CandidateOrder: []string{"psp_a"},
		},
		Candidates: []Candidate{eligibleCandidate("psp_a")},
	}
}

func eligibleCandidate(candidateID string) Candidate {
	return Candidate{
		ID:                      candidateID,
		ConnectionRevision:      candidateID + "_v1",
		ProviderNamespaceID:     candidateID,
		Provider:                candidateID,
		MerchantAccount:         "acct_" + candidateID,
		Connector:               candidateID + "_connector",
		Region:                  "us-east-1",
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
