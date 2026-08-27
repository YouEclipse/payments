package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"
)

var (
	identifier   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	candidateID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	countryCode  = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)
)

type compiledBucket struct {
	Bucket
	coverage uint32
}

// Snapshot owns its copies; no map or mutable slice escapes. A new artifact
// produces a new Snapshot and digest, never an in-place historical correction.
// The publisher must enforce version-to-digest uniqueness across publications.
type Snapshot struct {
	artifact   Artifact
	digest     string
	candidates map[string]string
	buckets    map[SegmentKey]compiledBucket
}

func Compile(input Artifact) (*Snapshot, error) {
	bad := func(message string) (*Snapshot, error) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArtifact, message)
	}
	if input.SchemaVersion != SchemaVersion || input.SegmentSchemaVersion != SegmentSchemaVersion ||
		input.EstimatorVersion != EstimatorVersion || !identifier.MatchString(input.Version) ||
		!validCountryField(input.CountryField) || !validCohort(input.Cohort) ||
		len(input.Candidates) == 0 || len(input.Candidates) > 128 || len(input.Buckets) > 32768 ||
		len(input.MinSamples) != 4 || input.MinCoveragePPM == 0 || input.MinCoveragePPM > uint32(ProbabilityScale) {
		return bad("unsupported schema, cohort or unbounded manifest")
	}
	if !utcInstant(input.WindowStart) || !utcInstant(input.WindowEnd) || !utcInstant(input.ObservedAt) || !utcInstant(input.ProducedAt) ||
		!input.WindowStart.Before(input.WindowEnd) || input.ObservedAt.Before(input.WindowEnd) || input.ProducedAt.Before(input.ObservedAt) ||
		input.MaxAge <= 0 || input.MaxWindowAge <= 0 {
		return bad("invalid window, watermark, publication time or freshness policy")
	}
	if !finite(input.Prior.Probability) || math.Signbit(input.Prior.Probability) || input.Prior.Probability > 1 ||
		!finite(input.Prior.Strength) || input.Prior.Strength <= 0 || input.Prior.Strength > float64(maxExactSamples) {
		return bad("prior must have a finite probability and positive bounded strength")
	}
	for _, minimum := range input.MinSamples {
		if minimum == 0 || minimum > maxExactSamples {
			return bad("invalid per-level sample threshold")
		}
	}
	artifact := input
	artifact.WindowStart, artifact.WindowEnd = input.WindowStart.UTC(), input.WindowEnd.UTC()
	artifact.ObservedAt, artifact.ProducedAt = input.ObservedAt.UTC(), input.ProducedAt.UTC()
	artifact.MinSamples = append([]uint64(nil), input.MinSamples...)
	artifact.Candidates = append([]Candidate(nil), input.Candidates...)
	artifact.Buckets = append([]Bucket(nil), input.Buckets...)
	snapshot := &Snapshot{candidates: make(map[string]string), buckets: make(map[SegmentKey]compiledBucket)}
	for _, candidate := range artifact.Candidates {
		if !candidateID.MatchString(candidate.ID) || !candidateID.MatchString(candidate.ConnectionRevision) || snapshot.candidates[candidate.ID] != "" {
			return bad("invalid or duplicate candidate identity")
		}
		snapshot.candidates[candidate.ID] = candidate.ConnectionRevision
	}
	for _, bucket := range artifact.Buckets {
		key := bucket.Segment
		if key.ConnectionRevision == "" || snapshot.candidates[key.CandidateID] != key.ConnectionRevision || !validSegment(key) ||
			bucket.Successes > bucket.SampleSize || bucket.SampleSize > maxExactSamples || bucket.PendingCount > maxExactSamples {
			return bad("bucket has invalid identity, hierarchy or counts")
		}
		if _, exists := snapshot.buckets[key]; exists {
			return bad("duplicate bucket key")
		}
		snapshot.buckets[key] = compiledBucket{bucket, coveragePPM(bucket.SampleSize, bucket.PendingCount)}
	}
	sort.Slice(artifact.Candidates, func(i, j int) bool { return artifact.Candidates[i].ID < artifact.Candidates[j].ID })
	sort.Slice(artifact.Buckets, func(i, j int) bool {
		return segmentOrder(artifact.Buckets[i].Segment) < segmentOrder(artifact.Buckets[j].Segment)
	})
	canonical, err := json.Marshal(artifact)
	if err != nil {
		return bad("artifact cannot be canonically encoded")
	}
	digest := sha256.Sum256(canonical)
	snapshot.artifact, snapshot.digest = artifact, "sha256:"+hex.EncodeToString(digest[:])
	return snapshot, nil
}

func (s *Snapshot) Version() string {
	if s == nil {
		return ""
	}
	return s.artifact.Version
}

func (s *Snapshot) Digest() string {
	if s == nil {
		return ""
	}
	return s.digest
}

func (s *Snapshot) Candidates() []Candidate {
	if s == nil {
		return nil
	}
	return append([]Candidate(nil), s.artifact.Candidates...)
}

func validCohort(cohort Cohort) bool {
	if cohort.PaymentMethod != "card" || cohort.AttemptKind != "first" || cohort.DenominatorVersion != DenominatorVersion {
		return false
	}
	return (cohort.Operation == "sale" && cohort.CaptureMethod == "automatic" && cohort.OutcomeDefinition == SaleOutcome) ||
		(cohort.Operation == "authorize" && cohort.CaptureMethod == "manual" && cohort.OutcomeDefinition == AuthorizationOutcome)
}

func validSegment(key SegmentKey) bool {
	if key.Currency == Wildcard {
		return key.CountryValue == Wildcard && key.NetworkValue == Wildcard
	}
	if !currencyCode.MatchString(key.Currency) {
		return false
	}
	if key.CountryValue == Wildcard {
		return key.NetworkValue == Wildcard
	}
	return countryCode.MatchString(key.CountryValue) && (key.NetworkValue == Wildcard || validNetwork(key.NetworkValue))
}

func validCountryField(value string) bool { return value == CountryMerchant || value == CountryIssuer }

func validNetwork(value string) bool {
	switch value {
	case "visa", "mastercard", "amex", "discover", "jcb", "unionpay", "diners":
		return true
	default:
		return false
	}
}

func segmentOrder(key SegmentKey) string {
	return key.CandidateID + "\x00" + key.ConnectionRevision + "\x00" + key.CountryValue + "\x00" + key.NetworkValue + "\x00" + key.Currency
}

func utcInstant(at time.Time) bool {
	_, offset := at.Zone()
	return !at.IsZero() && offset == 0 && at.Year() >= 1 && at.Year() <= 9999
}
