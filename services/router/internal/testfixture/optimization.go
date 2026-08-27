package testfixture

import (
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/router/internal/cost"
	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/performance"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/scoring"
)

// Optimization is the non-secret local v3 fixture. Both feature artifacts
// carry the exact Catalog identities even though hard eligibility normally
// leaves only one connection for the demo USD/EUR requests.
func Optimization() decision.OptimizationConfig {
	candidates := []cost.Candidate{{ID: "conn_us", ConnectionRevision: "conn_us_v1"}, {ID: "conn_eu", ConnectionRevision: "conn_eu_v1"}}
	fees := cost.Artifact{
		SchemaVersion: cost.SchemaVersion, Version: "fees_v1", CurrencyMetadataVersion: "currencies_v1", PublishedAt: Now.Add(-time.Hour),
		Candidates: candidates, Currencies: []cost.Currency{{Code: "EUR", Exponent: 2}, {Code: "USD", Exponent: 2}},
		FXQuotes: []cost.FXQuote{{Version: "eur_usd_v1", Source: "fixture_contract", BaseCurrency: "EUR", QuoteCurrency: "USD", Numerator: 11, Denominator: 10, AsOf: Now.Add(-time.Hour), ExpiresAt: Now.Add(time.Hour), Rounding: cost.RoundHalfUp}},
	}
	for index, candidate := range candidates {
		fixed := int64(300_000)
		variable := int64(250)
		if index == 1 {
			fixed, variable = 100_000, 200
		}
		for _, currency := range []string{"EUR", "USD"} {
			fees.Schedules = append(fees.Schedules, cost.FeeSchedule{
				ID: "fee_" + candidate.ID + "_" + currency, Version: "fee_v1", CandidateID: candidate.ID, ConnectionRevision: candidate.ConnectionRevision,
				Operation: "sale", PaymentCurrency: currency, FixedCurrency: "USD", EffectiveFrom: Now.Add(-time.Hour), EffectiveUntil: Now.Add(time.Hour), PercentageRound: cost.RoundHalfUp,
				Attempt: cost.Fee{FixedMicros: fixed}, Success: cost.Fee{VariableBPS: variable}, Failure: cost.Fee{FixedMicros: 50_000},
			})
		}
	}
	performanceCandidates := []performance.Candidate{{ID: "conn_us", ConnectionRevision: "conn_us_v1"}, {ID: "conn_eu", ConnectionRevision: "conn_eu_v1"}}
	performanceSnapshot := performance.Artifact{
		SchemaVersion: performance.SchemaVersion, Version: "performance_v1", SegmentSchemaVersion: performance.SegmentSchemaVersion, EstimatorVersion: performance.EstimatorVersion,
		CountryField: performance.CountryMerchant,
		Cohort:       performance.Cohort{PaymentMethod: "card", Operation: "sale", CaptureMethod: "automatic", AttemptKind: "first", OutcomeDefinition: performance.SaleOutcome, DenominatorVersion: performance.DenominatorVersion},
		WindowStart:  Now.Add(-24 * time.Hour), WindowEnd: Now.Add(-30 * time.Minute), ObservedAt: Now.Add(-20 * time.Minute), ProducedAt: Now.Add(-10 * time.Minute),
		MaxAge: 2 * time.Hour, MaxWindowAge: 3 * time.Hour, MinSamples: []uint64{25, 50, 100, 100}, MinCoveragePPM: 950_000,
		Prior: performance.Prior{Probability: .85, Strength: 20}, Candidates: performanceCandidates,
		Buckets: []performance.Bucket{
			{Segment: performance.SegmentKey{CandidateID: "conn_us", ConnectionRevision: "conn_us_v1", CountryValue: performance.Wildcard, NetworkValue: performance.Wildcard, Currency: performance.Wildcard}, Successes: 900, SampleSize: 1000},
			{Segment: performance.SegmentKey{CandidateID: "conn_eu", ConnectionRevision: "conn_eu_v1", CountryValue: performance.Wildcard, NetworkValue: performance.Wildcard, Currency: performance.Wildcard}, Successes: 980, SampleSize: 1000},
		},
	}
	profile := scoring.Profile{
		SchemaVersion: scoring.SchemaVersion, Version: "scoring_v1", PublishedAt: Now.Add(-time.Hour), EffectiveFrom: Now.Add(-30 * time.Minute), ExpiresAt: Now.Add(time.Hour),
		QuoteCurrency: "USD", SuccessWeightBPS: 7000, CostWeightBPS: 3000, CostFloorMicros: 0, CostCeilingMicros: 2_000_000,
		Primary: scoring.PrimaryFull, OnUnavailable: scoring.StaticOrder,
	}
	return decision.OptimizationConfig{SchemaVersion: decision.OptimizationSchemaVersion, Version: "optimization_v1", FeeCatalog: fees, PerformanceSnapshot: performanceSnapshot, ScoringProfile: profile, CrossBorderRules: []decision.CrossBorderRule{}}
}

func RiskContext(action string) routingcontext.Context {
	context := Context()
	context.SchemaVersion = routingcontext.RiskSchemaVersion
	subject, err := context.RiskSubjectDigest()
	if err != nil {
		panic(err)
	}
	reasons := []string{}
	if action != riskdecision.Allow {
		reasons = []string{"fixture_risk_action"}
	}
	context.Risk = &riskdecision.Decision{
		SchemaVersion: riskdecision.SchemaVersion, DecisionID: "risk_fixture", SubjectDigest: subject, Action: action, ReasonCodes: reasons,
		Version: "risk_v1", Source: "fixture_risk", EvaluatedAt: context.BuiltAt.Add(-time.Minute), ExpiresAt: context.BuiltAt.Add(30 * time.Minute),
	}
	return context
}
