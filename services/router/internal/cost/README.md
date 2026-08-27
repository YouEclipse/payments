# Deterministic cost scenarios

`Compile(Artifact)` validates and owns a canonical, immutable snapshot. `Estimate(at, Query)` accepts an already eligible Candidate ID **and connection revision**, a positive Intent amount, its payment currency, the approved comparison currency, and a versioned cross-border classification. It never accesses a clock, credentials, network, database, or provider.

## Units and conditional charges

All `*Micros` values are **one millionth of the named currency's major unit**, not millionths of a minor unit. A BPS is 1/10,000. Minor-unit exponents come from the artifact's versioned metadata, not from a hardcoded two-decimal assumption. The supported exponent range is 0–6.

| Output | Included fees |
| --- | --- |
| `AttemptCostMicros` | `FeeSchedule.Attempt` only; charged for every logical attempt |
| `SucceededCostMicros` | Attempt + Success-only fees |
| `FailedCostMicros` | Attempt + Failure-only fees |

`FixedMicros` is denominated in `FixedCurrency`; percentage and cross-border fees use `PaymentCurrency` and the original amount. Every component is rounded in source-currency micros, then converted and rounded separately in quote-currency micros. FX is a directed **major-unit** ratio: `quote = base × numerator / denominator`. There is no inferred inverse, default 1:1 rate, or floating-point money calculation. This v1 requires a directed quote even for a zero component if its declared currency differs from the comparison currency.

The three outputs are conditional Expected Cost scenarios, not probabilities, invoices, settled fees, or costs per successful payment. Success and failure components are never added together. The calling scoring profile must explicitly select its scenario or approved expected-value model. Negative rebates, monthly tiers, retry-chain modeling, actual-cost attribution, and live provider FX execution are out of scope.

## Availability and identity

Schedules use `[effective_from, effective_until)`. Compilation rejects duplicate schedule identities, overlapping scopes, unsupported operations (only `sale` and `authorize` are accepted), unbound revisions, invalid currency metadata, future-observed FX relative to publication, and overlapping FX quotes. A query before artifact publication cannot use that artifact for a past decision. No global clock is read.

Any conditional cross-border fee, including a failure-only fee, requires a `known` classification whose rule version equals the schedule's `CrossBorderRuleVersion`. A domestic value from another contract's classification is not trusted. Unknown, redacted, inapplicable, or stale facts cannot silently become domestic. Classification is not inferred from country inequality, tokens, PAN, or provider names.

`Result.Estimate` is non-nil **only** for `StateKnown`. Missing or stale evidence returns a reason and no estimate; malformed queries and arithmetic overflow additionally return an error. No partial/zero total escapes on failure. Legitimate zero fees still carry their schedule and artifact evidence. Comparison/degradation of the complete eligible batch remains the caller's responsibility.

Artifact arrays are copied and canonically sorted; `Version`, `Digest`, and defensive-copy `Candidates` support engine binding and replay. The publisher must enforce version-to-digest uniqueness across deployments; this pure package does not maintain a global version registry. The external JSON loader must reject unknown/duplicate JSON fields before constructing the typed artifact.

## Verification

```bash
go test -count=1 -race ./services/router/internal/cost
go vet ./services/router/internal/cost
go test ./services/router/internal/cost -run '^$' -fuzz '^FuzzFixedPointDoesNotWrap$' -fuzztime=10s -parallel=1
```

Tests cover price crossing, failure scenarios, half-open transitions, classification absence, directed FX, overflow, exact-rational properties, canonical ordering, immutable ownership, and concurrent replay. None is evidence of live-provider pricing or settlement accuracy.
