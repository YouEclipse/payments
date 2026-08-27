# Versioned observational performance

`Compile(Artifact)` owns an immutable snapshot; `Lookup(at, Query)` reads it at an explicit instant. Both use only the standard library. There is no event ingestion, database, model service, global clock, random routing, or provider execution.

## Cohort and evidence contract

Each artifact names one card first-attempt cohort, outcome definition, denominator version, geographic meaning, segment schema, estimator, window, outcome watermark, and production time. Candidate IDs **and revisions** are exact at every hierarchy level.

| Cohort | Finalized sample denominator |
| --- | --- |
| `sale` / `automatic` / `sale_first_attempt_completion.v1` | Definitive logical first-attempt SALE completions, including finalized technical failures |
| `authorize` / `manual` / `authorization_approval.v1` | Definitive issuer/network approvals and declines; connector technical errors are separate |

Neither includes local eligibility/risk blocks, transport redeliveries, follow-on Capture/Void/Refund, or unresolved UNKNOWN as a finalized failure. `PendingCount` is separate and reduces `sample_size / (sample_size + pending_count)` coverage. The package checks aggregate consistency; a producer must still establish truthful outcome normalization and logical-operation deduplication. Supporting an authorization statistics schema does not implement a manual-capture payment workflow.

## Fixed lookup, not the most attractive bucket

The hierarchy is fixed by `SegmentSchemaVersion`:

```text
L0 candidate+revision + country + network + currency
L1 candidate+revision + country + currency
L2 candidate+revision + currency
L3 candidate+revision
L4 declared global prior, with zero observed samples
```

`CountryField` explicitly means `merchant.country` or `payment_method_context.issuer_country` and must match the artifact. Country and network each carry state/value. Unknown, redacted, stale, and inapplicable observations skip the affected levels with reasons; they never become known empty keys. Unsupported wildcard shapes, duplicate buckets, and cross-revision buckets are rejected. Lookup cannot try several sibling segmentations and pick the highest observed rate.

Each level must meet its declared `MinSamples` threshold and `MinCoveragePPM`. The selected bucket shrinks toward qualified parents along the same chain, ending at the artifact prior: `(successes + strength × parent_probability) / (samples + strength)`. The raw sample retains its [Wilson score interval](https://www.itl.nist.gov/div898/handbook/prc/section2/prc241.htm); it is **not** the shrunk estimate's posterior interval. `ConservativeProbability = min(shrunk_probability, observed_lower_95)` is an explicitly conservative feature, not a guarantee. Integer PPM projections floor to the 0–1,000,000 range; no currency arithmetic uses these floating-point statistics.

Confidence thresholds belong to `EstimatorVersion`: high requires exact-level evidence, at least 1,000 samples, interval width ≤0.05 and coverage ≥0.99; medium requires a level above candidate-only, at least 100 samples, width ≤0.20 and coverage ≥0.95. Other qualified buckets are low confidence. Counts and prior strength are bounded at 2^53 before binary64 calculation.

## Degradation and authority

Both publication age and data-window age are checked. Republishing an old window does not refresh its data authority. Future/stale/mismatched artifacts return no numeric estimate. A cold start returns `StatePrior`, `Source=global_prior`, zero samples, `[0,1]` observed interval and a zero conservative PPM; it cannot masquerade as known candidate history. The caller must explicitly approve any prior-based scoring profile.

Every result states `historical_policy_conditional_not_causal` and carries `performance_observational_only`. Shrinkage and error bars do not remove historical routing bias, provide counterfactual support, or authorize exploration. Eligibility and whole-batch profile selection remain outside this package.

All arrays are copied; canonical digests and returned evidence are order-independent and safe for concurrent reads. The publisher owns version-to-digest uniqueness and the external loader owns strict raw-JSON field validation.

## Verification

```bash
go test -count=1 -race ./services/router/internal/performance
go vet ./services/router/internal/performance
go test ./services/router/internal/performance -run '^$' -fuzz '^FuzzStatisticalEvidenceRemainsBounded$' -fuzztime=10s -parallel=1
```

Tests exercise small samples, fixed fallback, pending coverage, unknown metadata, stale windows, revision and outcome isolation, immutable late-evidence versions, randomized numeric properties, artifact permutations and concurrent replay. Fixtures are synthetic and are not PSP benchmarks.
