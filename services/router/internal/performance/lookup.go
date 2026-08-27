package performance

import "time"

// Lookup evaluates a frozen Context slice at an explicit instant. It never
// reads a global clock and never searches sibling buckets for a higher rate.
func (s *Snapshot) Lookup(at time.Time, q Query) (Result, error) {
	result := Result{
		CandidateID: q.CandidateID, ConnectionRevision: q.ConnectionRevision,
		State: StateInvalid, ReasonCodes: []string{"performance_observational_only"},
		Interpretation: ObservationalOnly, SnapshotVersion: s.Version(), SnapshotDigest: s.Digest(),
		Source: "none", FallbackLevel: -1,
	}
	fail := func(state, reason string, err error) (Result, error) {
		result.State = state
		result.Estimate, result.SourceSegment, result.ParentSegment = nil, nil, nil
		result.Source, result.FallbackLevel = "none", -1
		result.ReasonCodes = addReason(result.ReasonCodes, reason)
		return result, err
	}
	if s == nil || s.digest == "" {
		return fail(StateInvalid, "performance_invalid_artifact", ErrInvalidArtifact)
	}
	artifact := s.artifact
	result.CountryField, result.Cohort = artifact.CountryField, artifact.Cohort
	result.WindowStart, result.WindowEnd, result.ObservedAt, result.ProducedAt = artifact.WindowStart, artifact.WindowEnd, artifact.ObservedAt, artifact.ProducedAt
	if !validQuery(q) || !utcInstant(at) {
		return fail(StateInvalid, "performance_invalid_query", ErrInvalidQuery)
	}
	result.EvaluatedAt = at.UTC()
	if at.Before(artifact.ProducedAt) {
		return fail(StateStale, "performance_snapshot_not_yet_published", nil)
	}
	if at.Sub(artifact.ProducedAt) > artifact.MaxAge || at.Sub(artifact.WindowEnd) > artifact.MaxWindowAge {
		return fail(StateStale, "performance_snapshot_stale", nil)
	}
	revision, exists := s.candidates[q.CandidateID]
	if !exists {
		return fail(StateUnknown, "performance_unknown_candidate", nil)
	}
	if revision != q.ConnectionRevision {
		return fail(StateUnknown, "performance_candidate_revision_mismatch", nil)
	}
	if q.CountryField != artifact.CountryField || q.Operation != artifact.Cohort.Operation || q.CaptureMethod != artifact.Cohort.CaptureMethod {
		return fail(StateUnknown, "performance_cohort_mismatch", nil)
	}
	countryKnown, networkKnown := q.CountryState == StateKnown, q.NetworkState == StateKnown
	if !countryKnown {
		result.ReasonCodes = addReason(result.ReasonCodes, "performance_country_"+q.CountryState)
	}
	if !networkKnown {
		result.ReasonCodes = addReason(result.ReasonCodes, "performance_network_"+q.NetworkState)
	}
	keys := [4]SegmentKey{
		{q.CandidateID, q.ConnectionRevision, q.CountryValue, q.NetworkValue, q.Currency},
		{q.CandidateID, q.ConnectionRevision, q.CountryValue, Wildcard, q.Currency},
		{q.CandidateID, q.ConnectionRevision, Wildcard, Wildcard, q.Currency},
		{q.CandidateID, q.ConnectionRevision, Wildcard, Wildcard, Wildcard},
	}
	var usable [4]bool
	var buckets [4]compiledBucket
	for level, key := range keys {
		if (level == 0 && (!countryKnown || !networkKnown)) || (level == 1 && !countryKnown) {
			continue
		}
		bucket, exists := s.buckets[key]
		if !exists {
			result.ReasonCodes = addReason(result.ReasonCodes, "performance_bucket_missing")
			continue
		}
		if bucket.SampleSize < artifact.MinSamples[level] {
			result.ReasonCodes = addReason(result.ReasonCodes, "performance_insufficient_sample")
			continue
		}
		if bucket.coverage < artifact.MinCoveragePPM {
			result.ReasonCodes = addReason(result.ReasonCodes, "performance_incomplete_outcomes")
			continue
		}
		usable[level], buckets[level] = true, bucket
	}
	// Parents are traversed only along this same hierarchy. A qualified parent
	// shrinks toward its qualified ancestor; a missing/sparse/incomplete parent
	// cannot be selected merely because its observed rate looks attractive.
	parentProbability := artifact.Prior.Probability
	var parent *SegmentKey
	for level := 3; level >= 0; level-- {
		if !usable[level] {
			continue
		}
		bucket := buckets[level]
		value, err := estimate(bucket.Successes, bucket.SampleSize, parentProbability, artifact.Prior.Strength)
		if err != nil {
			return fail(StateInvalid, "performance_invalid_evidence", err)
		}
		value.PendingCount, value.CoveragePPM = bucket.PendingCount, bucket.coverage
		value.Confidence = confidence(value, level)
		key := keys[level]
		result.Estimate, result.SourceSegment, result.ParentSegment = &value, &key, parent
		result.FallbackLevel = level
		parentProbability, parent = value.Probability, &key
	}
	if result.Estimate != nil {
		result.State, result.Source = StateKnown, "bucket"
		if result.FallbackLevel != 0 {
			result.ReasonCodes = addReason(result.ReasonCodes, "performance_hierarchy_fallback")
		}
		return result, nil
	}
	prior, err := estimate(0, 0, artifact.Prior.Probability, artifact.Prior.Strength)
	if err != nil {
		return fail(StateInvalid, "performance_invalid_evidence", err)
	}
	prior.Confidence = "prior_only"
	result.State, result.Source, result.FallbackLevel, result.Estimate = StatePrior, "global_prior", 4, &prior
	result.ReasonCodes = addReason(result.ReasonCodes, "performance_cold_start")
	return result, nil
}

// Thresholds are part of EstimatorVersion. Broad fallback is not high
// confidence solely because its unrelated contexts supplied many samples.
func confidence(value Estimate, level int) string {
	width := value.ObservedUpper95 - value.ObservedLower95
	if level == 0 && value.SampleSize >= 1000 && width <= 0.05 && value.CoveragePPM >= 990_000 {
		return "high"
	}
	if level < 3 && value.SampleSize >= 100 && width <= 0.20 && value.CoveragePPM >= 950_000 {
		return "medium"
	}
	return "low"
}

func validQuery(q Query) bool {
	if !candidateID.MatchString(q.CandidateID) || !candidateID.MatchString(q.ConnectionRevision) ||
		!validCountryField(q.CountryField) || !currencyCode.MatchString(q.Currency) ||
		!((q.Operation == "sale" && q.CaptureMethod == "automatic") || (q.Operation == "authorize" && q.CaptureMethod == "manual")) {
		return false
	}
	return validObservation(q.CountryState, q.CountryValue, countryCode.MatchString) && validObservation(q.NetworkState, q.NetworkValue, validNetwork)
}

func validObservation(state, value string, valid func(string) bool) bool {
	switch state {
	case StateKnown:
		return valid(value)
	case StateUnknown, StateRedacted, StateNotApplicable:
		return value == ""
	case StateStale:
		return value == "" || valid(value)
	default:
		return false
	}
}

func addReason(reasons []string, value string) []string {
	for _, existing := range reasons {
		if existing == value {
			return reasons
		}
	}
	return append(reasons, value)
}
