package decision_test

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/performance"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/scoring"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func optimizationConfig() decision.Config {
	optimization := testfixture.Optimization()
	return decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog(), Optimization: &optimization}
}

func admitBothUSD(config *decision.Config) {
	for index := range config.CapabilityCatalog.Descriptors {
		config.CapabilityCatalog.Descriptors[index].Currencies = testfixture.Known("USD")
	}
}

func TestOptimizationScoresOnlyExactAllowedIdentitiesAndCanReverseStaticOrder(t *testing.T) {
	config := optimizationConfig()
	admitBothUSD(&config)
	engine := newEngine(t, config)
	if !engine.OptimizationConfigured() {
		t.Fatal("v3 loader cannot distinguish an optimization engine")
	}
	result, err := engine.Decide(testfixture.RiskContext(riskdecision.Allow))
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk == nil || !result.Risk.Continue || result.Optimization == nil || result.Optimization.Scoring.Status != scoring.StatusScored {
		t.Fatalf("missing risk or optimization evidence: %+v", result)
	}
	if !reflect.DeepEqual(result.CandidateOrder, []string{"conn_eu", "conn_us"}) || result.SelectedCandidate == nil || result.SelectedCandidate.ID != "conn_eu" {
		t.Fatalf("score did not reverse the static order: %+v", result.CandidateOrder)
	}
	if result.SelectedCandidate.ConnectionRevision != "conn_eu_v1" || result.SelectedCandidate.Provider != "simulator_a" {
		t.Fatal("winner lost its exact execution identity")
	}
	if len(result.Optimization.Scoring.Breakdown) != 2 || len(result.Optimization.Classifications) != 2 {
		t.Fatal("comparison omitted an allowed identity")
	}
	for _, row := range result.Optimization.Scoring.Breakdown {
		if row.Numbers == nil || row.ConnectionRevision != row.Cost.ConnectionRevision || row.ConnectionRevision != row.Performance.ConnectionRevision {
			t.Fatal("score row mixed connection revisions")
		}
	}
	if !engine.ExecutableAt(result, testfixture.Now) {
		t.Fatal("fresh scored evidence is not executable")
	}
}

func TestEveryNonAllowRiskActionStopsBeforeFeatureEvaluation(t *testing.T) {
	for _, action := range []string{riskdecision.Block, riskdecision.Require3DS, riskdecision.ManualReview} {
		t.Run(action, func(t *testing.T) {
			config := optimizationConfig()
			context := testfixture.RiskContext(action)
			// This amount would overflow percentage cost arithmetic if a blocked
			// payment reached feature evaluation.
			context.Base.AmountMinor = math.MaxInt64
			subject, err := context.RiskSubjectDigest()
			if err != nil {
				t.Fatal(err)
			}
			context.Risk.SubjectDigest = subject
			result, err := newEngine(t, config).Decide(context)
			if !errors.Is(err, routing.ErrNoEligibleProvider) {
				t.Fatalf("risk action reached scoring or acquired a winner: %v", err)
			}
			if result.Risk == nil || result.Risk.Action != action || result.Risk.Continue || result.Optimization != nil || result.SelectedCandidate != nil || len(result.CandidateOrder) != 0 {
				t.Fatalf("risk fail-closed evidence is incomplete: %+v", result)
			}
		})
	}
}

func TestOptimizationManifestAndClassificationBindExactRevision(t *testing.T) {
	for name, edit := range map[string]func(*decision.Config){
		"fee revision": func(config *decision.Config) {
			config.Optimization.FeeCatalog.Candidates[1].ConnectionRevision = "conn_eu_v2"
			for index := range config.Optimization.FeeCatalog.Schedules {
				if config.Optimization.FeeCatalog.Schedules[index].CandidateID == "conn_eu" {
					config.Optimization.FeeCatalog.Schedules[index].ConnectionRevision = "conn_eu_v2"
				}
			}
		},
		"performance revision": func(config *decision.Config) {
			config.Optimization.PerformanceSnapshot.Candidates[1].ConnectionRevision = "conn_eu_v2"
			for index := range config.Optimization.PerformanceSnapshot.Buckets {
				if config.Optimization.PerformanceSnapshot.Buckets[index].Segment.CandidateID == "conn_eu" {
					config.Optimization.PerformanceSnapshot.Buckets[index].Segment.ConnectionRevision = "conn_eu_v2"
				}
			}
		},
		"classification revision": func(config *decision.Config) {
			config.Optimization.CrossBorderRules = []decision.CrossBorderRule{{CandidateID: "conn_eu", ConnectionRevision: "conn_eu_v2", RuleVersion: "cross_v1", CountryField: performance.CountryMerchant, CounterpartyCountry: "US", Source: "fixture", SourceVersion: "v1", ObservedAt: testfixture.Now.Add(-time.Hour), ExpiresAt: testfixture.Now.Add(time.Hour)}}
		},
		"performance currency": func(config *decision.Config) {
			config.Optimization.PerformanceSnapshot.Buckets[0].Segment.Currency = "JPY"
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := optimizationConfig()
			edit(&config)
			if engine, err := decision.NewEngine(config); engine != nil || !errors.Is(err, decision.ErrInvalidOptimization) {
				t.Fatalf("unbound exact revision compiled: %v", err)
			}
		})
	}
	config := optimizationConfig()
	for index := range config.Optimization.FeeCatalog.Schedules {
		schedule := &config.Optimization.FeeCatalog.Schedules[index]
		if schedule.CandidateID == "conn_us" && schedule.PaymentCurrency == "USD" {
			schedule.Success.CrossBorderBPS, schedule.CrossBorderRuleVersion = 100, "cross_v2"
		}
	}
	config.Optimization.CrossBorderRules = []decision.CrossBorderRule{{CandidateID: "conn_us", ConnectionRevision: "conn_us_v1", RuleVersion: "cross_v1", CountryField: performance.CountryMerchant, CounterpartyCountry: "US", Source: "fixture", SourceVersion: "v1", ObservedAt: testfixture.Now.Add(-time.Hour), ExpiresAt: testfixture.Now.Add(time.Hour)}}
	if engine, err := decision.NewEngine(config); engine != nil || !errors.Is(err, decision.ErrInvalidOptimization) {
		t.Fatalf("fee and geography versions drifted without rejection: %v", err)
	}
}

func TestUnavailableFeatureIsAWholeBatchAction(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*decision.Config)
	}{
		{"missing one fee", func(config *decision.Config) {
			config.Optimization.FeeCatalog.Schedules = slices.DeleteFunc(config.Optimization.FeeCatalog.Schedules, func(schedule cost.FeeSchedule) bool {
				return schedule.CandidateID == "conn_eu" && schedule.PaymentCurrency == "USD"
			})
		}},
		{"stale performance", func(config *decision.Config) { config.Optimization.PerformanceSnapshot.MaxAge = time.Minute }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := optimizationConfig()
			admitBothUSD(&config)
			test.edit(&config)
			engine := newEngine(t, config)
			result, err := engine.Decide(testfixture.RiskContext(riskdecision.Allow))
			if err != nil || result.Optimization == nil || result.Optimization.Scoring.Status != scoring.StatusStaticFallback || result.Optimization.Scoring.Mode != scoring.StaticOrder {
				t.Fatalf("unavailable evidence did not take declared whole-batch fallback: %+v %v", result, err)
			}
			if !reflect.DeepEqual(result.CandidateOrder, []string{"conn_us", "conn_eu"}) {
				t.Fatal("static fallback did not restore frozen rule order")
			}
			for _, row := range result.Optimization.Scoring.Breakdown {
				if row.Numbers != nil {
					t.Fatal("one candidate retained a score in a partial feature space")
				}
			}
			if !engine.ExecutableAt(result, testfixture.Now) {
				t.Fatal("declared static-order fallback incorrectly depended on stale soft evidence")
			}
		})
	}
	config := optimizationConfig()
	admitBothUSD(&config)
	config.Optimization.ScoringProfile.OnUnavailable = scoring.Reject
	config.Optimization.FeeCatalog.Schedules = nil
	result, err := newEngine(t, config).Decide(testfixture.RiskContext(riskdecision.Allow))
	if !errors.Is(err, routing.ErrNoEligibleProvider) || result.Optimization == nil || result.Optimization.Scoring.Status != scoring.StatusRejected || result.SelectedCandidate != nil || len(result.CandidateOrder) != 0 {
		t.Fatal("required features silently fell back", result, err)
	}
}

func TestUnknownCrossBorderNeverBecomesZeroCostDomestic(t *testing.T) {
	config := optimizationConfig()
	for index := range config.Optimization.FeeCatalog.Schedules {
		schedule := &config.Optimization.FeeCatalog.Schedules[index]
		if schedule.CandidateID == "conn_us" && schedule.PaymentCurrency == "USD" {
			schedule.Success.CrossBorderBPS, schedule.CrossBorderRuleVersion = 100, "issuer_contract_v1"
		}
	}
	config.Optimization.CrossBorderRules = []decision.CrossBorderRule{{
		CandidateID: "conn_us", ConnectionRevision: "conn_us_v1", RuleVersion: "issuer_contract_v1", CountryField: performance.CountryIssuer,
		CounterpartyCountry: "US", Source: "contract_registry", SourceVersion: "v1", ObservedAt: testfixture.Now.Add(-time.Hour), ExpiresAt: testfixture.Now.Add(time.Hour),
	}}
	result, err := newEngine(t, config).Decide(testfixture.RiskContext(riskdecision.Allow))
	if err != nil || result.Optimization == nil || result.Optimization.Scoring.Status != scoring.StatusStaticFallback {
		t.Fatalf("unknown geography did not abstain from scoring: %+v %v", result, err)
	}
	classification := result.Optimization.Classifications[0]
	row := result.Optimization.Scoring.Breakdown[0]
	if classification.State != cost.FactUnknown || classification.Value != nil || classification.Rule == nil || classification.Country == nil || row.Cost.State != cost.StateUnknown || row.Cost.Estimate != nil || !slices.Contains(row.Cost.ReasonCodes, "cost_unknown_cross_border") {
		t.Fatalf("unknown geography became a numeric domestic fee: %+v %+v", classification, row.Cost)
	}
}

func TestOptimizationCannotReviveAnEligibilityOrRuleExclusion(t *testing.T) {
	config := optimizationConfig()
	result, err := newEngine(t, config).Decide(testfixture.RiskContext(riskdecision.Allow))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.CandidateOrder, []string{"conn_us"}) || result.Optimization == nil || len(result.Optimization.Scoring.Breakdown) != 1 || result.Optimization.Scoring.Breakdown[0].CandidateID != "conn_us" {
		t.Fatal("scoring revived the currency-ineligible high-performing connection")
	}
	context := testfixture.RiskContext(riskdecision.Allow)
	context.Base.Currency = "GBP"
	subject, err := context.RiskSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	context.Risk.SubjectDigest = subject
	result, err = newEngine(t, config).Decide(context)
	if !errors.Is(err, routing.ErrNoEligibleProvider) || result.Risk == nil || result.Optimization != nil || result.SelectedCandidate != nil {
		t.Fatal("empty hard-allowed set reached feature lookup", result, err)
	}
}

func TestExecutableAtRechecksTheWholeScoredBatchAfterEvaluation(t *testing.T) {
	config := optimizationConfig()
	admitBothUSD(&config)
	for index := range config.Optimization.FeeCatalog.Schedules {
		schedule := &config.Optimization.FeeCatalog.Schedules[index]
		if schedule.CandidateID == "conn_us" && schedule.PaymentCurrency == "USD" {
			schedule.EffectiveUntil = testfixture.Now.Add(10 * time.Microsecond)
		}
	}
	engine := newEngine(t, config)
	result, err := engine.Decide(testfixture.RiskContext(riskdecision.Allow))
	if err != nil || result.SelectedCandidate == nil || result.SelectedCandidate.ID != "conn_eu" {
		t.Fatal("fixture must make the soon-expiring connection the losing comparator", result, err)
	}
	if want := testfixture.Now.Add(10 * time.Microsecond); !result.ExecutableUntil.Equal(want) {
		t.Fatalf("decision deadline=%s want losing comparator schedule deadline %s", result.ExecutableUntil, want)
	}
	if !engine.ExecutableAt(result, testfixture.Now.Add(9*time.Microsecond)) || engine.ExecutableAt(result, testfixture.Now.Add(10*time.Microsecond)) {
		t.Fatal("losing comparison evidence outlived its half-open fee schedule")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var replay decision.Decision
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatal(err)
	}
	if engine.ExecutableAt(replay, testfixture.Now) {
		t.Fatal("deserialized decision recreated process-local execution authority")
	}
}

func TestExecutableAtRechecksFXClassificationAndPerformance(t *testing.T) {
	rebindRisk := func(t *testing.T, context *routingcontext.Context) {
		t.Helper()
		subject, err := context.RiskSubjectDigest()
		if err != nil {
			t.Fatal(err)
		}
		context.Risk.SubjectDigest = subject
	}
	for _, test := range []struct {
		name  string
		build func(*testing.T) (decision.Config, routingcontext.Context)
	}{
		{"fx", func(t *testing.T) (decision.Config, routingcontext.Context) {
			config := optimizationConfig()
			config.Optimization.FeeCatalog.FXQuotes[0].ExpiresAt = testfixture.Now.Add(10 * time.Microsecond)
			context := testfixture.RiskContext(riskdecision.Allow)
			context.Base.Currency = "EUR"
			rebindRisk(t, &context)
			return config, context
		}},
		{"classification", func(t *testing.T) (decision.Config, routingcontext.Context) {
			config := optimizationConfig()
			for index := range config.Optimization.FeeCatalog.Schedules {
				schedule := &config.Optimization.FeeCatalog.Schedules[index]
				if schedule.CandidateID == "conn_us" && schedule.PaymentCurrency == "USD" {
					schedule.Success.CrossBorderBPS, schedule.CrossBorderRuleVersion = 100, "merchant_contract_v1"
				}
			}
			config.Optimization.CrossBorderRules = []decision.CrossBorderRule{{CandidateID: "conn_us", ConnectionRevision: "conn_us_v1", RuleVersion: "merchant_contract_v1", CountryField: performance.CountryMerchant, CounterpartyCountry: "CA", Source: "contract_registry", SourceVersion: "v1", ObservedAt: testfixture.Now.Add(-time.Hour), ExpiresAt: testfixture.Now.Add(10 * time.Microsecond)}}
			return config, testfixture.RiskContext(riskdecision.Allow)
		}},
		{"performance", func(t *testing.T) (decision.Config, routingcontext.Context) {
			config := optimizationConfig()
			config.Optimization.PerformanceSnapshot.MaxAge = 10*time.Minute + 10*time.Microsecond
			return config, testfixture.RiskContext(riskdecision.Allow)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, context := test.build(t)
			engine := newEngine(t, config)
			result, err := engine.Decide(context)
			if err != nil || result.Optimization == nil || result.Optimization.Scoring.Status != scoring.StatusScored {
				t.Fatalf("fresh dependency did not score: %+v %v", result, err)
			}
			if !engine.ExecutableAt(result, testfixture.Now.Add(9*time.Microsecond)) || engine.ExecutableAt(result, testfixture.Now.Add(10*time.Microsecond)) {
				t.Fatal("dependency expiry was extended after evaluation")
			}
		})
	}
}

func TestOptimizationSnapshotDigestOrderOwnershipAndConcurrentReplay(t *testing.T) {
	config := optimizationConfig()
	admitBothUSD(&config)
	context := testfixture.RiskContext(riskdecision.Allow)
	engine := newEngine(t, config)
	baseline, err := engine.Decide(context)
	if err != nil {
		t.Fatal(err)
	}
	reordered := optimizationConfig()
	admitBothUSD(&reordered)
	slices.Reverse(reordered.Optimization.FeeCatalog.Candidates)
	slices.Reverse(reordered.Optimization.FeeCatalog.Currencies)
	slices.Reverse(reordered.Optimization.FeeCatalog.Schedules)
	slices.Reverse(reordered.Optimization.PerformanceSnapshot.Candidates)
	slices.Reverse(reordered.Optimization.PerformanceSnapshot.Buckets)
	second, err := newEngine(t, reordered).Decide(context)
	if err != nil || !reflect.DeepEqual(baseline, second) {
		t.Fatal("equivalent artifact ordering changed evidence", err)
	}
	config.Optimization.FeeCatalog.Schedules[0].Attempt.FixedMicros = math.MaxInt64
	config.Optimization.PerformanceSnapshot.Buckets[0].Successes = 0
	config.Optimization.ScoringProfile.SuccessWeightBPS = 0
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := engine.Decide(context)
			if err != nil || !reflect.DeepEqual(result, baseline) {
				t.Error("compiled optimization aliased input or raced", err)
			}
		}()
	}
	group.Wait()
	changed := optimizationConfig()
	admitBothUSD(&changed)
	changed.Optimization.ScoringProfile.Version = "scoring_v2"
	changedResult, err := newEngine(t, changed).Decide(context)
	if err != nil {
		t.Fatal(err)
	}
	if changedResult.Optimization.ConfigDigest == baseline.Optimization.ConfigDigest || changedResult.ID == baseline.ID {
		t.Fatal("behavioral optimization change was absent from digests")
	}
}

func TestOptimizationRejectsUnsupportedContextWithoutChangingLegacyEngine(t *testing.T) {
	legacy := newEngine(t, config())
	if legacy.OptimizationConfigured() {
		t.Fatal("nil optimization changed v2 semantics")
	}
	if _, err := legacy.Decide(testfixture.Context()); err != nil {
		t.Fatal(err)
	}
	optimized := newEngine(t, optimizationConfig())
	if _, err := optimized.Decide(testfixture.Context()); !errors.Is(err, decision.ErrInvalidOptimization) {
		t.Fatal("optimization accepted routing_context.v1", err)
	}
	context := testfixture.RiskContext(riskdecision.Allow)
	context.Base.Operation, context.Base.CaptureMethod = "authorize", "manual"
	subject, err := context.RiskSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	context.Risk.SubjectDigest = subject
	if _, err := optimized.Decide(context); !errors.Is(err, decision.ErrInvalidOptimization) {
		t.Fatal("sale optimizer accepted an authorization cohort", err)
	}
}

func TestCrossBorderEvidenceOwnsContextPointers(t *testing.T) {
	config := optimizationConfig()
	for index := range config.Optimization.FeeCatalog.Schedules {
		schedule := &config.Optimization.FeeCatalog.Schedules[index]
		if schedule.CandidateID == "conn_us" && schedule.PaymentCurrency == "USD" {
			schedule.Success.CrossBorderBPS, schedule.CrossBorderRuleVersion = 100, "merchant_contract_v1"
		}
	}
	config.Optimization.CrossBorderRules = []decision.CrossBorderRule{{CandidateID: "conn_us", ConnectionRevision: "conn_us_v1", RuleVersion: "merchant_contract_v1", CountryField: performance.CountryMerchant, CounterpartyCountry: "CA", Source: "contract_registry", SourceVersion: "v1", ObservedAt: testfixture.Now.Add(-time.Hour), ExpiresAt: testfixture.Now.Add(time.Hour)}}
	context := testfixture.RiskContext(riskdecision.Allow)
	result, err := newEngine(t, config).Decide(context)
	if err != nil || result.Optimization == nil || result.Optimization.Classifications[0].Value == nil || !*result.Optimization.Classifications[0].Value {
		t.Fatal("known named countries were not classified", result, err)
	}
	*context.Merchant.Country.Value = "CA"
	if *result.Optimization.Classifications[0].Country.Value != "US" || !*result.Optimization.Classifications[0].Value {
		t.Fatal("returned classification aliased the caller Context")
	}
}
