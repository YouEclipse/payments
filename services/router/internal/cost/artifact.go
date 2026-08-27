package cost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

var (
	identifier  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	candidateID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	currencyID  = regexp.MustCompile(`^[A-Z]{3}$`)
)

type scheduleKey struct{ candidate, revision, operation, currency string }
type fxKey struct{ base, quote string }

// Evaluator owns canonical copies of all artifact data. Its maps and slices
// are private and never mutated after Compile, so concurrent reads are safe.
type Evaluator struct {
	artifact   Artifact
	digest     string
	candidates map[string]string
	currencies map[string]uint8
	schedules  map[scheduleKey][]FeeSchedule
	fx         map[fxKey][]FXQuote
}

func Compile(input Artifact) (*Evaluator, error) {
	bad := func(message string) (*Evaluator, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArtifact, message)
	}
	if input.SchemaVersion != SchemaVersion || !identifier.MatchString(input.Version) ||
		!identifier.MatchString(input.CurrencyMetadataVersion) || !utcInstant(input.PublishedAt) ||
		len(input.Candidates) == 0 || len(input.Candidates) > 128 ||
		len(input.Currencies) == 0 || len(input.Currencies) > 256 ||
		len(input.Schedules) > 8192 || len(input.FXQuotes) > 4096 {
		return bad("unsupported schema, invalid provenance or unbounded manifest")
	}
	artifact := input
	artifact.PublishedAt = input.PublishedAt.UTC()
	artifact.Candidates = append([]Candidate(nil), input.Candidates...)
	artifact.Currencies = append([]Currency(nil), input.Currencies...)
	artifact.Schedules = append([]FeeSchedule(nil), input.Schedules...)
	artifact.FXQuotes = append([]FXQuote(nil), input.FXQuotes...)
	e := &Evaluator{
		candidates: make(map[string]string), currencies: make(map[string]uint8),
		schedules: make(map[scheduleKey][]FeeSchedule), fx: make(map[fxKey][]FXQuote),
	}
	for _, candidate := range artifact.Candidates {
		if !candidateID.MatchString(candidate.ID) || !candidateID.MatchString(candidate.ConnectionRevision) || e.candidates[candidate.ID] != "" {
			return bad("invalid or duplicate candidate identity")
		}
		e.candidates[candidate.ID] = candidate.ConnectionRevision
	}
	for _, currency := range artifact.Currencies {
		if !currencyID.MatchString(currency.Code) || currency.Exponent > 6 {
			return bad("invalid currency metadata")
		}
		if _, exists := e.currencies[currency.Code]; exists {
			return bad("duplicate currency metadata")
		}
		e.currencies[currency.Code] = currency.Exponent
	}
	seenSchedules := make(map[[2]string]bool)
	for i := range artifact.Schedules {
		schedule := &artifact.Schedules[i]
		versionKey := [2]string{schedule.ID, schedule.Version}
		if !identifier.MatchString(schedule.ID) || !identifier.MatchString(schedule.Version) || seenSchedules[versionKey] ||
			schedule.ConnectionRevision == "" || e.candidates[schedule.CandidateID] != schedule.ConnectionRevision ||
			!validOperation(schedule.Operation) || !validRounding(schedule.PercentageRound) ||
			!validInterval(schedule.EffectiveFrom, schedule.EffectiveUntil) {
			return bad("invalid, duplicate or unbound fee schedule")
		}
		seenSchedules[versionKey] = true
		if _, exists := e.currencies[schedule.PaymentCurrency]; !exists {
			return bad("fee schedule payment currency lacks versioned metadata")
		}
		if _, exists := e.currencies[schedule.FixedCurrency]; !exists {
			return bad("fixed-fee currency lacks versioned metadata")
		}
		for _, fee := range []Fee{schedule.Attempt, schedule.Success, schedule.Failure} {
			if fee.FixedMicros < 0 || fee.VariableBPS < 0 || fee.CrossBorderBPS < 0 {
				return bad("negative fees or rebates are not supported")
			}
		}
		needsClassification := schedule.Attempt.CrossBorderBPS != 0 || schedule.Success.CrossBorderBPS != 0 || schedule.Failure.CrossBorderBPS != 0
		if (needsClassification && !identifier.MatchString(schedule.CrossBorderRuleVersion)) ||
			(schedule.CrossBorderRuleVersion != "" && !identifier.MatchString(schedule.CrossBorderRuleVersion)) {
			return bad("conditional fees must bind a canonical cross-border rule version")
		}
		schedule.EffectiveFrom = schedule.EffectiveFrom.UTC()
		schedule.EffectiveUntil = schedule.EffectiveUntil.UTC()
		key := scheduleKey{schedule.CandidateID, schedule.ConnectionRevision, schedule.Operation, schedule.PaymentCurrency}
		e.schedules[key] = append(e.schedules[key], *schedule)
	}
	for key, schedules := range e.schedules {
		sort.Slice(schedules, func(i, j int) bool { return schedules[i].EffectiveFrom.Before(schedules[j].EffectiveFrom) })
		for i := 1; i < len(schedules); i++ {
			if schedules[i].EffectiveFrom.Before(schedules[i-1].EffectiveUntil) {
				return bad("fee schedule effective intervals overlap")
			}
		}
		e.schedules[key] = schedules
	}
	for i := range artifact.FXQuotes {
		quote := &artifact.FXQuotes[i]
		if !identifier.MatchString(quote.Version) || !identifier.MatchString(quote.Source) ||
			quote.BaseCurrency == quote.QuoteCurrency || quote.Numerator <= 0 || quote.Denominator <= 0 ||
			!validRounding(quote.Rounding) || !validInterval(quote.AsOf, quote.ExpiresAt) || quote.AsOf.After(artifact.PublishedAt) {
			return bad("FX requires a positive directed rate and nonfuture observation")
		}
		if _, exists := e.currencies[quote.BaseCurrency]; !exists {
			return bad("FX base currency lacks metadata")
		}
		if _, exists := e.currencies[quote.QuoteCurrency]; !exists {
			return bad("FX quote currency lacks metadata")
		}
		quote.AsOf, quote.ExpiresAt = quote.AsOf.UTC(), quote.ExpiresAt.UTC()
		key := fxKey{quote.BaseCurrency, quote.QuoteCurrency}
		e.fx[key] = append(e.fx[key], *quote)
	}
	for key, quotes := range e.fx {
		sort.Slice(quotes, func(i, j int) bool { return quotes[i].AsOf.Before(quotes[j].AsOf) })
		seenVersions := make(map[string]bool)
		for i, quote := range quotes {
			if seenVersions[quote.Version] || (i > 0 && quote.AsOf.Before(quotes[i-1].ExpiresAt)) {
				return bad("FX versions are duplicated or effective intervals overlap")
			}
			seenVersions[quote.Version] = true
		}
		e.fx[key] = quotes
	}
	// Canonical ordering is independent of configuration input order. Internal
	// evaluation never iterates a map to choose a schedule, quote, or candidate.
	sort.Slice(artifact.Candidates, func(i, j int) bool { return artifact.Candidates[i].ID < artifact.Candidates[j].ID })
	sort.Slice(artifact.Currencies, func(i, j int) bool { return artifact.Currencies[i].Code < artifact.Currencies[j].Code })
	sort.Slice(artifact.Schedules, func(i, j int) bool {
		a, b := artifact.Schedules[i], artifact.Schedules[j]
		if a.CandidateID != b.CandidateID {
			return a.CandidateID < b.CandidateID
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		if a.PaymentCurrency != b.PaymentCurrency {
			return a.PaymentCurrency < b.PaymentCurrency
		}
		if !a.EffectiveFrom.Equal(b.EffectiveFrom) {
			return a.EffectiveFrom.Before(b.EffectiveFrom)
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Version < b.Version
	})
	sort.Slice(artifact.FXQuotes, func(i, j int) bool {
		a, b := artifact.FXQuotes[i], artifact.FXQuotes[j]
		if a.BaseCurrency != b.BaseCurrency {
			return a.BaseCurrency < b.BaseCurrency
		}
		if a.QuoteCurrency != b.QuoteCurrency {
			return a.QuoteCurrency < b.QuoteCurrency
		}
		if !a.AsOf.Equal(b.AsOf) {
			return a.AsOf.Before(b.AsOf)
		}
		return a.Version < b.Version
	})
	canonical, err := json.Marshal(artifact)
	if err != nil {
		return bad("artifact cannot be canonically encoded")
	}
	digest := sha256.Sum256(canonical)
	e.artifact, e.digest = artifact, "sha256:"+hex.EncodeToString(digest[:])
	return e, nil
}

func (e *Evaluator) Version() string {
	if e == nil {
		return ""
	}
	return e.artifact.Version
}

func (e *Evaluator) Digest() string {
	if e == nil {
		return ""
	}
	return e.digest
}

func (e *Evaluator) Candidates() []Candidate {
	if e == nil {
		return nil
	}
	return append([]Candidate(nil), e.artifact.Candidates...)
}

func validOperation(operation string) bool { return operation == "sale" || operation == "authorize" }

func utcInstant(at time.Time) bool {
	_, offset := at.Zone()
	return !at.IsZero() && offset == 0 && at.Year() >= 1 && at.Year() <= 9999
}

func validInterval(from, until time.Time) bool {
	return utcInstant(from) && utcInstant(until) && from.Before(until)
}
