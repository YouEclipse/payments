package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/performance"
	"github.com/YouEclipse/payments/services/router/internal/scoring"
	"github.com/YouEclipse/payments/services/router/internal/strictjson"
)

func loadV3Engine(environment, rawJSON string, now time.Time) (*decision.Engine, error) {
	if strings.TrimSpace(rawJSON) == "" {
		if !isDevelopmentEnvironment(environment) {
			return nil, nil
		}
		config := developmentV2Config()
		optimization := developmentOptimizationConfig(config, now.UTC())
		config.Optimization = &optimization
		return decision.NewEngine(config)
	}
	var config decision.Config
	if err := strictjson.Decode([]byte(rawJSON), &config); err != nil {
		return nil, fmt.Errorf("decode ROUTER_V3_CONFIG_JSON: %w", err)
	}
	if config.Optimization == nil {
		return nil, fmt.Errorf("decode ROUTER_V3_CONFIG_JSON: optimization is required")
	}
	return decision.NewEngine(config)
}

// This is deliberately a short-lived local Simulator fixture. It demonstrates
// the complete contract but does not assert real PSP prices, causal success
// rates, production FX authority, or live fraud capability.
func developmentOptimizationConfig(config decision.Config, now time.Time) decision.OptimizationConfig {
	costCandidates := make([]cost.Candidate, 0, len(config.CapabilityCatalog.Candidates))
	performanceCandidates := make([]performance.Candidate, 0, len(config.CapabilityCatalog.Candidates))
	for _, candidate := range config.CapabilityCatalog.Candidates {
		costCandidates = append(costCandidates, cost.Candidate{ID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision})
		performanceCandidates = append(performanceCandidates, performance.Candidate{ID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision})
	}
	fees := cost.Artifact{
		SchemaVersion: cost.SchemaVersion, Version: "local_fees_v1", CurrencyMetadataVersion: "local_currency_metadata_v1",
		PublishedAt: now.Add(-time.Hour), Candidates: costCandidates,
		Currencies: []cost.Currency{{Code: "BRL", Exponent: 2}, {Code: "EUR", Exponent: 2}, {Code: "USD", Exponent: 2}},
		FXQuotes: []cost.FXQuote{
			{Version: "local_brl_usd_v1", Source: "local_simulator_contract", BaseCurrency: "BRL", QuoteCurrency: "USD", Numerator: 1, Denominator: 5, AsOf: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour), Rounding: cost.RoundHalfUp},
			{Version: "local_eur_usd_v1", Source: "local_simulator_contract", BaseCurrency: "EUR", QuoteCurrency: "USD", Numerator: 11, Denominator: 10, AsOf: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour), Rounding: cost.RoundHalfUp},
		},
	}
	for index, candidate := range costCandidates {
		fixed, variable := int64(300_000), int64(250)
		if index == 1 {
			fixed, variable = 100_000, 200
		}
		for _, currency := range []string{"BRL", "EUR", "USD"} {
			fees.Schedules = append(fees.Schedules, cost.FeeSchedule{
				ID: "local_fee_" + candidate.ID + "_" + strings.ToLower(currency), Version: "local_fee_v1",
				CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, Operation: "sale",
				PaymentCurrency: currency, FixedCurrency: "USD", EffectiveFrom: now.Add(-time.Hour), EffectiveUntil: now.Add(24 * time.Hour), PercentageRound: cost.RoundHalfUp,
				Attempt: cost.Fee{FixedMicros: fixed}, Success: cost.Fee{VariableBPS: variable}, Failure: cost.Fee{FixedMicros: 50_000},
			})
		}
	}
	performanceSnapshot := performance.Artifact{
		SchemaVersion: performance.SchemaVersion, Version: "local_performance_v1", SegmentSchemaVersion: performance.SegmentSchemaVersion,
		EstimatorVersion: performance.EstimatorVersion, CountryField: performance.CountryMerchant,
		Cohort:      performance.Cohort{PaymentMethod: "card", Operation: "sale", CaptureMethod: "automatic", AttemptKind: "first", OutcomeDefinition: performance.SaleOutcome, DenominatorVersion: performance.DenominatorVersion},
		WindowStart: now.Add(-24 * time.Hour), WindowEnd: now.Add(-30 * time.Minute), ObservedAt: now.Add(-20 * time.Minute), ProducedAt: now.Add(-10 * time.Minute),
		MaxAge: 2 * time.Hour, MaxWindowAge: 3 * time.Hour, MinSamples: []uint64{25, 50, 100, 100}, MinCoveragePPM: 950_000,
		Prior: performance.Prior{Probability: .85, Strength: 20}, Candidates: performanceCandidates,
	}
	for index, candidate := range performanceCandidates {
		successes := uint64(900)
		if index == 1 {
			successes = 980
		}
		performanceSnapshot.Buckets = append(performanceSnapshot.Buckets, performance.Bucket{
			Segment:   performance.SegmentKey{CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision, CountryValue: performance.Wildcard, NetworkValue: performance.Wildcard, Currency: performance.Wildcard},
			Successes: successes, SampleSize: 1000,
		})
	}
	profile := scoring.Profile{
		SchemaVersion: scoring.SchemaVersion, Version: "local_scoring_v1", PublishedAt: now.Add(-time.Hour), EffectiveFrom: now.Add(-30 * time.Minute), ExpiresAt: now.Add(24 * time.Hour),
		QuoteCurrency: "USD", SuccessWeightBPS: 7000, CostWeightBPS: 3000, CostFloorMicros: 0, CostCeilingMicros: 2_000_000,
		Primary: scoring.PrimaryFull, OnUnavailable: scoring.StaticOrder,
	}
	return decision.OptimizationConfig{SchemaVersion: decision.OptimizationSchemaVersion, Version: "local_optimization_v1", FeeCatalog: fees, PerformanceSnapshot: performanceSnapshot, ScoringProfile: profile, CrossBorderRules: []decision.CrossBorderRule{}}
}
