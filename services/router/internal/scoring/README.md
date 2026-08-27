# Router scoring

`scoring` ranks a complete batch that has already passed Context validation,
eligibility, rule policy, and Risk. It has no network, storage, global clock,
provider-execution, or eligibility authority. The package cannot add a candidate
or change a connection revision; it only returns an order over the exact
`Batch.Allowed` identity set.

## Versioned profile

`Compile(Profile)` validates and snapshots an immutable `scoring_profile.v1`:

- a publication instant and half-open `[effective_from, expires_at)` lifetime;
- one quote currency and fixed cost floor/ceiling in millionths of its major
  unit;
- success and cost weights in basis points that sum to 10,000;
- primary mode `full_v1` and a whole-batch unavailable action of either
  `static_order` or `reject`.

The returned digest binds every profile field. `ValidAt(at)` and `Score(at,
batch)` require an explicit UTC instant; an unpublished, not-yet-effective, or
expired profile is an error and cannot enter fallback.

`full_v1` requires both features for every candidate, including a feature with
zero weight. It intentionally has no `success_only`, `cost_only`, implicit
zero, or per-candidate degradation mode.

## Complete evidence contract

Every allowed candidate needs exactly one `cost.Result` and one
`performance.Result` with the same candidate ID and connection revision.
Known rows must retain the feature artifact version, digest, calculation
definition, source window, and raw breakdown. Within a batch:

- cost artifacts and currency-metadata versions must agree;
- performance artifact, geography definition, cohort, and observation window
  must agree;
- known performance source segments may fall back through `*`, but conflicting
  concrete country, network, or currency values are rejected;
- directed FX items must reproduce `source_micros * numerator / denominator`
  with their declared rounding mode and validity interval;
- every feature evaluation instant must equal the scoring instant.

An explicit cost `unknown`/`stale` or performance
`unknown`/`stale`/`prior` is valid unavailability. It invokes the profile's
whole-batch action, and every row has `Numbers == nil`. A missing row, duplicate,
invalid state, malformed amount, inconsistent PPM, mixed contract, or other
invalid metadata returns `ErrInvalidBatch` with an empty order even when the
profile declares `static_order`.

Feature artifact digests are replay/provenance identifiers, not signatures.
Artifact loading and publisher authentication remain caller responsibilities.
The caller must derive both feature queries from one frozen, validated payment
Context; scoring verifies the comparable evidence carried in their results but
does not reconstruct the original Intent.

## Integer score

The objective is explicitly
`conservative_success_and_success_scenario_cost.v1`:

- success uses `performance.Estimate.ConservativePPM` and converts to 0–10,000
  points with half-up rounding;
- cost uses `cost.Estimate.SucceededCostMicros`, clamps it to the fixed profile
  interval, and projects it inversely to 0–10,000 points with floor rounding;
- each weighted contribution floors independently, then the two contributions
  are added.

The cost interval is profile-wide, not candidate-set min/max, so an irrelevant
candidate cannot renormalize existing candidates. The calculation uses bounded
integers and exact wide intermediates. It is not expected utility, success-cost
ratio, retry cost, or causal uplift. Failed-attempt cost remains in the raw cost
breakdown but is not silently combined with a probability.

The total order is:

1. total points descending;
2. frozen rule rank ascending;
3. candidate ID ascending.

`Result.Breakdown` is canonical by candidate ID and retains raw cost and
performance evidence plus all normalization inputs and contributions. Returned
nested data is owned by the result and does not alias the input batch.

## Verification

Use a workspace-owned Go temporary directory when repository policy requires
all generated artifacts to remain under the workspace:

```sh
: "${SCORING_GOTMPDIR:?set SCORING_GOTMPDIR to a directory inside the workspace}"
mkdir -p "$SCORING_GOTMPDIR"
GOTMPDIR="$SCORING_GOTMPDIR" go test -race ./services/router/internal/scoring
GOTMPDIR="$SCORING_GOTMPDIR" go vet ./services/router/internal/scoring
```

Tests build feature results through the real `cost` and `performance` packages,
cover integer golden values, malformed provenance, whole-batch abstention,
prior/stale/unknown evidence, context and FX consistency, extreme `int64`
normalization, input permutations, ownership, and concurrent reuse. The fuzz
target checks exact bounds and monotonicity without external dependencies.
