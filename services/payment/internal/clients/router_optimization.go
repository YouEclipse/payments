package clients

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

var errOptimizationEvidence = errors.New("invalid optimization evidence")

// ValidateOptimizedDecision is shared with Payment's transactional admission
// check. It verifies the already accepted Router envelope; it does not rerun or
// reinterpret Router-owned cost/statistical mathematics.
func ValidateOptimizedDecision(context routingcontext.Context, decision RoutingDecision, status int) error {
	return validateOptimizationDecision(context, decision, status)
}

// ValidVersionedDecisionID recomputes Router's credential-free canonical
// content identifier. Storage admission uses the same check after decoding the
// persisted snapshot so serialization or mutation cannot silently detach it.
func ValidVersionedDecisionID(decision RoutingDecision) bool {
	return validVersionedDecisionID(decision)
}

// VersionedDecisionContentID is exposed within Payment's internal boundary for
// contract fixtures and deterministic persistence verification. It is a
// content checksum, not an authentication primitive.
func VersionedDecisionContentID(decision RoutingDecision) string {
	return versionedDecisionContentID(decision)
}

func validVersionedDecisionID(decision RoutingDecision) bool {
	return decision.DecisionID == versionedDecisionContentID(decision)
}

func versionedDecisionContentID(decision RoutingDecision) string {
	var selected *RouteCandidate
	if decision.SelectedCandidate != (RouteCandidate{}) {
		copy := decision.SelectedCandidate
		selected = &copy
	}
	optimization := decision.Optimization
	if len(optimization) != 0 {
		var err error
		optimization, err = canonicalOptimizationJSON(optimization)
		if err != nil {
			return ""
		}
	}
	canonical := struct {
		ID                       string                 `json:"decision_id"`
		SelectedCandidate        *RouteCandidate        `json:"selected_candidate,omitempty"`
		CandidateOrder           []string               `json:"candidate_order"`
		Exclusions               []CandidateExclusion   `json:"exclusions"`
		PolicyVersion            string                 `json:"policy_version"`
		ContextSchemaVersion     string                 `json:"context_schema_version"`
		ContextDigest            string                 `json:"context_digest"`
		ExecutableUntil          time.Time              `json:"executable_until"`
		RuleSetVersion           string                 `json:"rule_set_version"`
		RuleSetDigest            string                 `json:"rule_set_digest"`
		CapabilityCatalogVersion string                 `json:"capability_catalog_version"`
		CapabilityCatalogDigest  string                 `json:"capability_catalog_digest"`
		RuleHits                 []RuleHit              `json:"rule_hits"`
		Eligibility              []EligibilityEvidence  `json:"eligibility"`
		Risk                     *riskdecision.Evidence `json:"risk,omitempty"`
		Optimization             json.RawMessage        `json:"optimization,omitempty"`
		ReasonCodes              []string               `json:"reason_codes"`
	}{"", selected, decision.CandidateOrder, decision.Exclusions, decision.PolicyVersion,
		decision.ContextSchemaVersion, decision.ContextDigest, decision.ExecutableUntil, decision.RuleSetVersion, decision.RuleSetDigest,
		decision.CapabilityCatalogVersion, decision.CapabilityCatalogDigest, decision.RuleHits, decision.Eligibility,
		decision.Risk, optimization, decision.ReasonCodes}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return "rtd_" + hex.EncodeToString(digest[:16])
}

// canonicalOptimizationJSON reconstructs Router's typed wire representation
// from semantic JSON. PostgreSQL jsonb deliberately does not preserve object
// member order, so hashing the stored RawMessage bytes would detach an
// otherwise unchanged v3 decision from its Router-issued content ID. Arrays
// remain ordered, every field remains bound, and ambiguous JSON is rejected
// before projection.
func canonicalOptimizationJSON(payload []byte) ([]byte, error) {
	if err := validateUniqueJSON(payload); err != nil {
		return nil, errOptimizationEvidence
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || !validOptimizationShape(value, "optimization", 0) || !validOptimizationState(value) {
		return nil, errOptimizationEvidence
	}
	result, err := appendCanonicalOptimization(nil, value, "optimization", 0)
	if err != nil {
		return nil, errOptimizationEvidence
	}
	return result, nil
}

func appendCanonicalOptimization(dst []byte, value any, shape string, depth int) ([]byte, error) {
	if depth > 16 {
		return nil, errOptimizationEvidence
	}
	if shape == "string" {
		item, ok := value.(string)
		if !ok {
			return nil, errOptimizationEvidence
		}
		return appendOptimizationJSON(dst, item)
	}
	if strings.HasPrefix(shape, "[]") {
		items, ok := value.([]any)
		if !ok {
			return nil, errOptimizationEvidence
		}
		dst = append(dst, '[')
		for index, item := range items {
			if index > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendCanonicalOptimization(dst, item, shape[2:], depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	}
	object, ok := value.(map[string]any)
	definition, known := optimizationShapes[shape]
	order, ordered := optimizationCanonicalFields[shape]
	if !ok || !known || !ordered || len(order) != len(definition) {
		return nil, errOptimizationEvidence
	}
	seen := make(map[string]bool, len(order))
	for _, name := range order {
		if seen[name] || definition[name] == "" {
			return nil, errOptimizationEvidence
		}
		seen[name] = true
	}
	dst = append(dst, '{')
	emitted := 0
	for _, name := range order {
		item, present := object[name]
		if !present {
			continue
		}
		if emitted > 0 {
			dst = append(dst, ',')
		}
		var err error
		dst, err = appendOptimizationJSON(dst, name)
		if err != nil {
			return nil, err
		}
		dst = append(dst, ':')
		child := definition[name]
		if child == "scalar" {
			dst, err = appendCanonicalOptimizationScalar(dst, item, optimizationFieldKind(shape, name))
		} else {
			dst, err = appendCanonicalOptimization(dst, item, child, depth+1)
		}
		if err != nil {
			return nil, err
		}
		emitted++
	}
	if emitted != len(object) {
		return nil, errOptimizationEvidence
	}
	return append(dst, '}'), nil
}

func appendCanonicalOptimizationScalar(dst []byte, value any, kind string) ([]byte, error) {
	switch kind {
	case optimizationKindString:
		item, ok := value.(string)
		if !ok {
			return nil, errOptimizationEvidence
		}
		return appendOptimizationJSON(dst, item)
	case optimizationKindInteger:
		item, ok := value.(json.Number)
		if !ok {
			return nil, errOptimizationEvidence
		}
		integer, err := item.Int64()
		if err != nil {
			return nil, errOptimizationEvidence
		}
		return strconv.AppendInt(dst, integer, 10), nil
	case optimizationKindNumber:
		item, ok := value.(json.Number)
		if !ok {
			return nil, errOptimizationEvidence
		}
		number, err := strconv.ParseFloat(string(item), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, errOptimizationEvidence
		}
		return appendOptimizationJSON(dst, number)
	case optimizationKindBoolean:
		item, ok := value.(bool)
		if !ok {
			return nil, errOptimizationEvidence
		}
		return strconv.AppendBool(dst, item), nil
	case optimizationKindTime:
		item, ok := value.(string)
		if !ok {
			return nil, errOptimizationEvidence
		}
		instant, err := time.Parse(time.RFC3339Nano, item)
		if err != nil {
			return nil, errOptimizationEvidence
		}
		return appendOptimizationJSON(dst, instant.UTC().Format(time.RFC3339Nano))
	default:
		return nil, errOptimizationEvidence
	}
}

func appendOptimizationJSON(dst []byte, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(dst, encoded...), nil
}

func validateOptimizationDecision(context routingcontext.Context, decision RoutingDecision, status int) error {
	if context.Risk == nil || decision.Risk == nil {
		return errOptimizationEvidence
	}
	subject, err := context.RiskSubjectDigest()
	if err != nil {
		return err
	}
	expected, err := riskdecision.EvaluateSale(context.BuiltAt, subject, *context.Risk)
	if err != nil {
		return err
	}
	want, _ := json.Marshal(expected)
	got, _ := json.Marshal(decision.Risk)
	if !bytes.Equal(want, got) {
		return errOptimizationEvidence
	}
	if !expected.Continue {
		if status != http.StatusUnprocessableEntity || len(decision.Optimization) != 0 || len(decision.CandidateOrder) != 0 || decision.SelectedCandidate != (RouteCandidate{}) {
			return errOptimizationEvidence
		}
		return nil
	}
	if len(decision.Optimization) == 0 {
		if status != http.StatusUnprocessableEntity {
			return errOptimizationEvidence
		}
		return nil // Eligibility or hard rules may already have emptied the set.
	}
	var object any
	decoder := json.NewDecoder(bytes.NewReader(decision.Optimization))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || !validOptimizationShape(object, "optimization", 0) || !validOptimizationState(object) {
		return errOptimizationEvidence
	}
	var envelope struct {
		ConfigVersion string `json:"config_version"`
		ConfigDigest  string `json:"config_digest"`
		Scoring       struct {
			Status           string    `json:"status"`
			Mode             string    `json:"mode"`
			CandidateOrder   []string  `json:"candidate_order"`
			ProfileVersion   string    `json:"profile_version"`
			ProfileDigest    string    `json:"profile_digest"`
			ObjectiveVersion string    `json:"objective_version"`
			EvaluatedAt      time.Time `json:"evaluated_at"`
			Breakdown        []struct {
				CandidateID    string `json:"candidate_id"`
				Revision       string `json:"connection_revision"`
				ProfileVersion string `json:"profile_version"`
				ProfileDigest  string `json:"profile_digest"`
				Cost           struct {
					CandidateID     string          `json:"candidate_id"`
					Revision        string          `json:"connection_revision"`
					State           string          `json:"state"`
					ArtifactVersion string          `json:"artifact_version"`
					ArtifactDigest  string          `json:"artifact_digest"`
					Estimate        json.RawMessage `json:"estimate"`
				} `json:"cost"`
				Performance struct {
					CandidateID     string          `json:"candidate_id"`
					Revision        string          `json:"connection_revision"`
					State           string          `json:"state"`
					SnapshotVersion string          `json:"snapshot_version"`
					SnapshotDigest  string          `json:"snapshot_digest"`
					Estimate        json.RawMessage `json:"estimate"`
				} `json:"performance"`
				Numbers *struct {
					TotalPoints int64 `json:"total_points"`
				} `json:"numbers"`
			} `json:"breakdown"`
		} `json:"scoring"`
		Classifications []struct {
			CandidateID string `json:"candidate_id"`
			Revision    string `json:"connection_revision"`
			State       string `json:"state"`
			Value       *bool  `json:"value"`
		} `json:"classifications"`
	}
	if err := json.Unmarshal(decision.Optimization, &envelope); err != nil {
		return errOptimizationEvidence
	}
	score := envelope.Scoring
	if envelope.ConfigVersion == "" || !validArtifactDigest(envelope.ConfigDigest) || score.ProfileVersion == "" ||
		!validArtifactDigest(score.ProfileDigest) || score.ObjectiveVersion != "conservative_success_and_success_scenario_cost.v1" ||
		!score.EvaluatedAt.Equal(context.BuiltAt) || !slices.Equal(score.CandidateOrder, decision.CandidateOrder) {
		return errOptimizationEvidence
	}
	if status == http.StatusUnprocessableEntity {
		if score.Status != "rejected" || score.Mode != "reject" || len(score.CandidateOrder) != 0 {
			return errOptimizationEvidence
		}
		return validateOptimizationRows(envelope.Scoring.Breakdown, envelope.Classifications, nil, "rejected")
	}
	if !((score.Status == "scored" && score.Mode == "full_v1") || (score.Status == "static_fallback" && score.Mode == "static_order")) || len(score.Breakdown) != len(score.CandidateOrder) {
		return errOptimizationEvidence
	}
	if err := validateOptimizationRows(score.Breakdown, envelope.Classifications, score.CandidateOrder, score.Status); err != nil {
		return err
	}
	seen := make(map[string]bool, len(score.Breakdown))
	for _, item := range score.Breakdown {
		if seen[item.CandidateID] || !slices.Contains(score.CandidateOrder, item.CandidateID) || !validPlatformIdentifier(item.Revision) ||
			item.ProfileVersion != score.ProfileVersion || item.ProfileDigest != score.ProfileDigest ||
			(item.CandidateID == decision.SelectedCandidate.CandidateID && item.Revision != decision.SelectedCandidate.ConnectionRevision) {
			return errOptimizationEvidence
		}
		seen[item.CandidateID] = true
		if score.Status == "scored" && (item.Numbers == nil || item.Numbers.TotalPoints < 0 || item.Numbers.TotalPoints > 10000) {
			return errOptimizationEvidence
		}
		if score.Status == "static_fallback" && item.Numbers != nil {
			return errOptimizationEvidence
		}
	}
	return nil
}

func validateOptimizationRows(rows []struct {
	CandidateID    string `json:"candidate_id"`
	Revision       string `json:"connection_revision"`
	ProfileVersion string `json:"profile_version"`
	ProfileDigest  string `json:"profile_digest"`
	Cost           struct {
		CandidateID     string          `json:"candidate_id"`
		Revision        string          `json:"connection_revision"`
		State           string          `json:"state"`
		ArtifactVersion string          `json:"artifact_version"`
		ArtifactDigest  string          `json:"artifact_digest"`
		Estimate        json.RawMessage `json:"estimate"`
	} `json:"cost"`
	Performance struct {
		CandidateID     string          `json:"candidate_id"`
		Revision        string          `json:"connection_revision"`
		State           string          `json:"state"`
		SnapshotVersion string          `json:"snapshot_version"`
		SnapshotDigest  string          `json:"snapshot_digest"`
		Estimate        json.RawMessage `json:"estimate"`
	} `json:"performance"`
	Numbers *struct {
		TotalPoints int64 `json:"total_points"`
	} `json:"numbers"`
}, classifications []struct {
	CandidateID string `json:"candidate_id"`
	Revision    string `json:"connection_revision"`
	State       string `json:"state"`
	Value       *bool  `json:"value"`
}, order []string, status string) error {
	seen := make(map[string]string, len(rows))
	anyUnavailable := false
	for _, row := range rows {
		if seen[row.CandidateID] != "" || !validPlatformIdentifier(row.CandidateID) || !validPlatformIdentifier(row.Revision) ||
			row.Cost.CandidateID != row.CandidateID || row.Cost.Revision != row.Revision ||
			row.Performance.CandidateID != row.CandidateID || row.Performance.Revision != row.Revision ||
			row.Cost.ArtifactVersion == "" || !validArtifactDigest(row.Cost.ArtifactDigest) ||
			row.Performance.SnapshotVersion == "" || !validArtifactDigest(row.Performance.SnapshotDigest) {
			return errOptimizationEvidence
		}
		if order != nil && !slices.Contains(order, row.CandidateID) {
			return errOptimizationEvidence
		}
		seen[row.CandidateID] = row.Revision
		costKnown, performanceKnown := row.Cost.State == "known", row.Performance.State == "known"
		anyUnavailable = anyUnavailable || !costKnown || !performanceKnown
		if !costKnown && row.Cost.State != "unknown" && row.Cost.State != "stale" {
			return errOptimizationEvidence
		}
		if !performanceKnown && row.Performance.State != "prior" && row.Performance.State != "unknown" && row.Performance.State != "stale" {
			return errOptimizationEvidence
		}
		if (costKnown != (len(row.Cost.Estimate) != 0)) || ((performanceKnown || row.Performance.State == "prior") != (len(row.Performance.Estimate) != 0)) {
			return errOptimizationEvidence
		}
		if status == "scored" && (!costKnown || !performanceKnown) {
			return errOptimizationEvidence
		}
	}
	if status == "static_fallback" && !anyUnavailable {
		return errOptimizationEvidence
	}
	if len(classifications) != len(rows) {
		return errOptimizationEvidence
	}
	classified := make(map[string]bool, len(classifications))
	for _, item := range classifications {
		if classified[item.CandidateID] || seen[item.CandidateID] != item.Revision ||
			(item.State != "known" && item.State != "unknown" && item.State != "stale" && item.State != "not_applicable") ||
			(item.State == "known") != (item.Value != nil) {
			return errOptimizationEvidence
		}
		classified[item.CandidateID] = true
	}
	return nil
}

// This allowlist preserves complete Router-owned evidence without accepting a
// future accidental credential/debug field through RawMessage. It is not a
// second cost or statistical model. Versioned DTO changes require reader-first
// rollout, just like Context; strings are bounded, and arrays are bounded by
// the already limited HTTP response size plus this per-node limit.
var optimizationCanonicalFields = map[string][]string{
	"optimization":         required("config_version config_digest scoring classifications"),
	"score":                required("status mode candidate_order reason_codes profile_version profile_digest objective_version evaluated_at breakdown"),
	"score_row":            required("candidate_id connection_revision rule_rank profile_version profile_digest cost performance numbers"),
	"numbers":              required("succeeded_cost_micros quote_currency conservative_ppm cost_floor_micros cost_ceiling_micros clamped_cost_micros success_weight_bps cost_weight_bps success_points cost_points success_contribution cost_contribution total_points rounding_version"),
	"cost":                 required("candidate_id connection_revision state reason_codes artifact_version artifact_digest estimate"),
	"cost_estimate":        required("quote_currency attempt_cost_micros succeeded_cost_micros failed_cost_micros breakdown schedule_id schedule_version currency_metadata_version cross_border_rule_version evaluated_at"),
	"cost_item":            required("scenario kind basis source_currency source_micros quote_currency amount_micros percentage_round cross_border_rule_version fx_version fx_source fx_numerator fx_denominator fx_rounding fx_as_of fx_expires_at"),
	"performance":          required("candidate_id connection_revision state reason_codes interpretation snapshot_version snapshot_digest country_field cohort source source_segment parent_segment fallback_level window_start window_end observed_at produced_at evaluated_at estimate"),
	"cohort":               required("payment_method operation capture_method attempt_kind outcome_definition denominator_version"),
	"segment":              required("candidate_id connection_revision country_value network_value currency"),
	"performance_estimate": required("probability probability_ppm conservative_probability conservative_ppm observed_lower_95 observed_upper_95 successes sample_size pending_count coverage_ppm confidence parent_probability prior_strength estimator_version interval_method_version"),
	"classification":       required("candidate_id connection_revision state value rule country evaluated_at reason_codes"),
	"classification_rule":  required("candidate_id connection_revision rule_version country_field counterparty_country source source_version observed_at expires_at"),
	"observed":             required("state value source source_version observed_at expires_at"),
}

var optimizationShapes = map[string]map[string]string{
	"optimization":         fields("config_version config_digest", map[string]string{"scoring": "score", "classifications": "[]classification"}),
	"score":                fields("status mode profile_version profile_digest objective_version evaluated_at", map[string]string{"candidate_order": "[]string", "reason_codes": "[]string", "breakdown": "[]score_row"}),
	"score_row":            fields("candidate_id connection_revision rule_rank profile_version profile_digest", map[string]string{"cost": "cost", "performance": "performance", "numbers": "numbers"}),
	"numbers":              fields("succeeded_cost_micros quote_currency conservative_ppm cost_floor_micros cost_ceiling_micros clamped_cost_micros success_weight_bps cost_weight_bps success_points cost_points success_contribution cost_contribution total_points rounding_version", nil),
	"cost":                 fields("candidate_id connection_revision state artifact_version artifact_digest", map[string]string{"reason_codes": "[]string", "estimate": "cost_estimate"}),
	"cost_estimate":        fields("quote_currency attempt_cost_micros succeeded_cost_micros failed_cost_micros schedule_id schedule_version currency_metadata_version cross_border_rule_version evaluated_at", map[string]string{"breakdown": "[]cost_item"}),
	"cost_item":            fields("scenario kind basis source_currency source_micros quote_currency amount_micros percentage_round cross_border_rule_version fx_version fx_source fx_numerator fx_denominator fx_rounding fx_as_of fx_expires_at", nil),
	"performance":          fields("candidate_id connection_revision state interpretation snapshot_version snapshot_digest country_field source fallback_level window_start window_end observed_at produced_at evaluated_at", map[string]string{"reason_codes": "[]string", "cohort": "cohort", "source_segment": "segment", "parent_segment": "segment", "estimate": "performance_estimate"}),
	"cohort":               fields("payment_method operation capture_method attempt_kind outcome_definition denominator_version", nil),
	"segment":              fields("candidate_id connection_revision country_value network_value currency", nil),
	"performance_estimate": fields("probability probability_ppm conservative_probability conservative_ppm observed_lower_95 observed_upper_95 successes sample_size pending_count coverage_ppm confidence parent_probability prior_strength estimator_version interval_method_version", nil),
	"classification":       fields("candidate_id connection_revision state value evaluated_at", map[string]string{"rule": "classification_rule", "country": "observed", "reason_codes": "[]string"}),
	"classification_rule":  fields("candidate_id connection_revision rule_version country_field counterparty_country source source_version observed_at expires_at", nil),
	"observed":             fields("state value source source_version observed_at expires_at", nil),
}

// Presence is part of the wire contract. Encoding into a Go zero value is not
// sufficient because it makes an omitted security-relevant field
// indistinguishable from an explicitly published zero. Fields not listed here
// are the state-dependent optional fields checked by validOptimizationState.
var optimizationRequired = map[string][]string{
	"optimization":         required("config_version config_digest scoring classifications"),
	"score":                required("status mode candidate_order reason_codes profile_version profile_digest objective_version evaluated_at breakdown"),
	"score_row":            required("candidate_id connection_revision rule_rank profile_version profile_digest cost performance"),
	"numbers":              required("succeeded_cost_micros quote_currency conservative_ppm cost_floor_micros cost_ceiling_micros clamped_cost_micros success_weight_bps cost_weight_bps success_points cost_points success_contribution cost_contribution total_points rounding_version"),
	"cost":                 required("candidate_id connection_revision state reason_codes artifact_version artifact_digest"),
	"cost_estimate":        required("quote_currency attempt_cost_micros succeeded_cost_micros failed_cost_micros breakdown schedule_id schedule_version currency_metadata_version evaluated_at"),
	"cost_item":            required("scenario kind basis source_currency source_micros quote_currency amount_micros"),
	"performance":          required("candidate_id connection_revision state reason_codes interpretation snapshot_version snapshot_digest country_field cohort source fallback_level window_start window_end observed_at produced_at evaluated_at"),
	"cohort":               required("payment_method operation capture_method attempt_kind outcome_definition denominator_version"),
	"segment":              required("candidate_id connection_revision country_value network_value currency"),
	"performance_estimate": required("probability probability_ppm conservative_probability conservative_ppm observed_lower_95 observed_upper_95 successes sample_size pending_count coverage_ppm confidence parent_probability prior_strength estimator_version interval_method_version"),
	"classification":       required("candidate_id connection_revision state evaluated_at reason_codes"),
	"classification_rule":  required("candidate_id connection_revision rule_version country_field counterparty_country source source_version observed_at expires_at"),
	"observed":             required("state source source_version observed_at"),
}

const (
	optimizationKindString  = "string"
	optimizationKindInteger = "integer"
	optimizationKindNumber  = "number"
	optimizationKindBoolean = "boolean"
	optimizationKindTime    = "time"
)

// Scalar wire types are explicit for fields that do not decode into the
// narrow identity projection above. Unlisted scalar fields are strings.
var optimizationScalarKinds = map[string]map[string]string{
	"score":     {"evaluated_at": optimizationKindTime},
	"score_row": {"rule_rank": optimizationKindInteger},
	"numbers": scalarKindMap(optimizationKindInteger,
		"succeeded_cost_micros conservative_ppm cost_floor_micros cost_ceiling_micros clamped_cost_micros success_weight_bps cost_weight_bps success_points cost_points success_contribution cost_contribution total_points"),
	"cost_estimate": mergeScalarKinds(
		scalarKindMap(optimizationKindInteger, "attempt_cost_micros succeeded_cost_micros failed_cost_micros"),
		scalarKindMap(optimizationKindTime, "evaluated_at")),
	"cost_item": mergeScalarKinds(
		scalarKindMap(optimizationKindInteger, "source_micros amount_micros fx_numerator fx_denominator"),
		scalarKindMap(optimizationKindTime, "fx_as_of fx_expires_at")),
	"performance": mergeScalarKinds(
		scalarKindMap(optimizationKindInteger, "fallback_level"),
		scalarKindMap(optimizationKindTime, "window_start window_end observed_at produced_at evaluated_at")),
	"performance_estimate": mergeScalarKinds(
		scalarKindMap(optimizationKindNumber, "probability conservative_probability observed_lower_95 observed_upper_95 parent_probability prior_strength"),
		scalarKindMap(optimizationKindInteger, "probability_ppm conservative_ppm successes sample_size pending_count coverage_ppm")),
	"classification":      {"value": optimizationKindBoolean, "evaluated_at": optimizationKindTime},
	"classification_rule": scalarKindMap(optimizationKindTime, "observed_at expires_at"),
	"observed":            scalarKindMap(optimizationKindTime, "observed_at expires_at"),
}

func scalarKindMap(kind, names string) map[string]string {
	result := make(map[string]string)
	for _, name := range strings.Fields(names) {
		result[name] = kind
	}
	return result
}

func mergeScalarKinds(values ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, value := range values {
		for name, kind := range value {
			result[name] = kind
		}
	}
	return result
}

func fields(names string, nested map[string]string) map[string]string {
	result := make(map[string]string)
	for _, name := range strings.Fields(names) {
		result[name] = "scalar"
	}
	for name, child := range nested {
		result[name] = child
	}
	return result
}

func required(names string) []string { return strings.Fields(names) }

func validOptimizationShape(value any, shape string, depth int) bool {
	if depth > 16 {
		return false
	}
	if shape == "scalar" {
		switch item := value.(type) {
		case string:
			return validOptimizationScalarString(item)
		case json.Number:
			// Typed numbers are interpreted by the field schema below. A Luhn
			// match is not credential evidence: legitimate counters and micros
			// can be 13-19 digits as well.
			return true
		case bool:
			return true
		default:
			return false
		}
	}
	if shape == "string" {
		item, ok := value.(string)
		return ok && validOptimizationScalarString(item)
	}
	if strings.HasPrefix(shape, "[]") {
		items, ok := value.([]any)
		if !ok || len(items) > 4096 {
			return false
		}
		for _, item := range items {
			if !validOptimizationShape(item, shape[2:], depth+1) {
				return false
			}
		}
		return true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	definition, known := optimizationShapes[shape]
	if !known {
		return false
	}
	for name, item := range object {
		child, allowed := definition[name]
		if !allowed || (child == "scalar" && !validOptimizationFieldScalar(shape, name, item)) || !validOptimizationShape(item, child, depth+1) {
			return false
		}
	}
	for _, name := range optimizationRequired[shape] {
		if _, present := object[name]; !present {
			return false
		}
	}
	return true
}

func validOptimizationFieldScalar(shape, name string, value any) bool {
	kind := optimizationFieldKind(shape, name)
	switch kind {
	case optimizationKindString:
		item, ok := value.(string)
		return ok && validOptimizationScalarString(item)
	case optimizationKindInteger:
		item, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := item.Int64()
		return err == nil
	case optimizationKindNumber:
		item, ok := value.(json.Number)
		if !ok {
			return false
		}
		number, err := strconv.ParseFloat(string(item), 64)
		return err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	case optimizationKindBoolean:
		_, ok := value.(bool)
		return ok
	case optimizationKindTime:
		item, ok := value.(string)
		if !ok || !validOptimizationScalarString(item) || !strings.HasSuffix(item, "Z") {
			return false
		}
		instant, err := time.Parse(time.RFC3339Nano, item)
		return err == nil && !instant.IsZero()
	default:
		return false
	}
}

func optimizationFieldKind(shape, name string) string {
	if fields, exists := optimizationScalarKinds[shape]; exists && fields[name] != "" {
		return fields[name]
	}
	return optimizationKindString
}

func validOptimizationState(value any) bool {
	root, ok := optimizationObject(value)
	if !ok {
		return false
	}
	score, ok := optimizationChildObject(root, "scoring")
	if !ok {
		return false
	}
	status, ok := optimizationString(score, "status")
	if !ok || (status != "scored" && status != "static_fallback" && status != "rejected") {
		return false
	}
	rows, ok := optimizationArray(score, "breakdown")
	if !ok {
		return false
	}
	for _, value := range rows {
		row, ok := optimizationObject(value)
		if !ok || optimizationPresent(row, "numbers") != (status == "scored") {
			return false
		}
		cost, ok := optimizationChildObject(row, "cost")
		if !ok || !validOptimizationCostState(cost) {
			return false
		}
		performance, ok := optimizationChildObject(row, "performance")
		if !ok || !validOptimizationPerformanceState(performance) {
			return false
		}
	}
	classifications, ok := optimizationArray(root, "classifications")
	if !ok {
		return false
	}
	for _, value := range classifications {
		classification, ok := optimizationObject(value)
		if !ok || !validOptimizationClassificationState(classification) {
			return false
		}
	}
	return true
}

func validOptimizationCostState(value map[string]any) bool {
	state, ok := optimizationString(value, "state")
	if !ok {
		return false
	}
	estimate, present := value["estimate"]
	switch state {
	case "known":
		if !present {
			return false
		}
	case "unknown", "stale":
		return !present
	default:
		return false
	}
	estimateObject, ok := optimizationObject(estimate)
	if !ok {
		return false
	}
	breakdown, ok := optimizationArray(estimateObject, "breakdown")
	if !ok {
		return false
	}
	usesCrossBorder := false
	for _, item := range breakdown {
		itemObject, ok := optimizationObject(item)
		if !ok || !validOptimizationCostItemState(itemObject) {
			return false
		}
		kind, _ := optimizationString(itemObject, "kind")
		usesCrossBorder = usesCrossBorder || kind == "cross_border"
	}
	return optimizationPresent(estimateObject, "cross_border_rule_version") == usesCrossBorder
}

func validOptimizationCostItemState(value map[string]any) bool {
	kind, kindOK := optimizationString(value, "kind")
	source, sourceOK := optimizationString(value, "source_currency")
	quote, quoteOK := optimizationString(value, "quote_currency")
	if !kindOK || !sourceOK || !quoteOK {
		return false
	}
	percentage, crossBorder := optimizationPresent(value, "percentage_round"), optimizationPresent(value, "cross_border_rule_version")
	switch kind {
	case "fixed":
		if percentage || crossBorder {
			return false
		}
	case "variable":
		if !percentage || crossBorder {
			return false
		}
	case "cross_border":
		if !percentage || !crossBorder {
			return false
		}
	default:
		return false
	}
	fxFields := []string{"fx_version", "fx_source", "fx_numerator", "fx_denominator", "fx_rounding", "fx_as_of", "fx_expires_at"}
	wantFX := source != quote
	for _, field := range fxFields {
		if optimizationPresent(value, field) != wantFX {
			return false
		}
	}
	return true
}

func validOptimizationPerformanceState(value map[string]any) bool {
	state, stateOK := optimizationString(value, "state")
	source, sourceOK := optimizationString(value, "source")
	level, levelOK := optimizationInteger(value, "fallback_level")
	if !stateOK || !sourceOK || !levelOK {
		return false
	}
	estimate := optimizationPresent(value, "estimate")
	sourceSegment := optimizationPresent(value, "source_segment")
	parentSegment := optimizationPresent(value, "parent_segment")
	switch state {
	case "known":
		return estimate && sourceSegment && source == "bucket" && level >= 0 && level <= 3
	case "prior":
		return estimate && !sourceSegment && !parentSegment && source == "global_prior" && level == 4
	case "unknown", "stale":
		return !estimate && !sourceSegment && !parentSegment && source == "none" && level == -1
	default:
		return false
	}
}

func validOptimizationClassificationState(value map[string]any) bool {
	state, ok := optimizationString(value, "state")
	if !ok {
		return false
	}
	hasValue := optimizationPresent(value, "value")
	hasRule := optimizationPresent(value, "rule")
	hasCountry := optimizationPresent(value, "country")
	if hasRule != hasCountry {
		return false
	}
	switch state {
	case "known":
		if !hasValue || !hasRule {
			return false
		}
	case "unknown", "stale":
		if hasValue {
			return false
		}
	case "not_applicable":
		if hasValue || hasRule {
			return false
		}
	default:
		return false
	}
	if !hasCountry {
		return true
	}
	country, ok := optimizationChildObject(value, "country")
	if !ok {
		return false
	}
	countryState, ok := optimizationString(country, "state")
	if !ok {
		return false
	}
	hasCountryValue := optimizationPresent(country, "value")
	hasCountryExpiry := optimizationPresent(country, "expires_at")
	switch countryState {
	case "known", "stale":
		return hasCountryValue && hasCountryExpiry
	case "unknown", "redacted", "not_applicable":
		return !hasCountryValue
	default:
		return false
	}
}

func optimizationObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func optimizationChildObject(value map[string]any, name string) (map[string]any, bool) {
	child, present := value[name]
	if !present {
		return nil, false
	}
	return optimizationObject(child)
}

func optimizationArray(value map[string]any, name string) ([]any, bool) {
	items, ok := value[name].([]any)
	return items, ok
}

func optimizationString(value map[string]any, name string) (string, bool) {
	item, ok := value[name].(string)
	return item, ok
}

func optimizationInteger(value map[string]any, name string) (int64, bool) {
	item, ok := value[name].(json.Number)
	if !ok {
		return 0, false
	}
	integer, err := item.Int64()
	return integer, err == nil
}

func optimizationPresent(value map[string]any, name string) bool {
	_, present := value[name]
	return present
}

func validOptimizationScalarString(value string) bool {
	if len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"pm_", "tok_", "sk_", "rk_"} {
		for start := strings.Index(lower, prefix); start >= 0; {
			if start == 0 || !isASCIILetterOrDigit(lower[start-1]) {
				return false
			}
			next := strings.Index(lower[start+1:], prefix)
			if next < 0 {
				break
			}
			start += next + 1
		}
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:+-*", character) {
			continue
		}
		return false
	}
	return !containsLikelyPAN(value)
}

func containsLikelyPAN(value string) bool {
	for start := 0; start < len(value); {
		if value[start] < '0' || value[start] > '9' {
			start++
			continue
		}
		digits := make([]byte, 0, 19)
		end := start
		for end < len(value) {
			character := value[end]
			if character >= '0' && character <= '9' {
				digits = append(digits, character)
				end++
				continue
			}
			if character == '-' || character == '.' || character == '_' || character == '*' {
				end++
				continue
			}
			break
		}
		if len(digits) >= 13 && len(digits) <= 19 && luhn(string(digits)) {
			return true
		}
		if end == start {
			start++
		} else {
			start = end
		}
	}
	return false
}

func isASCIILetterOrDigit(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9')
}

func luhn(value string) bool {
	sum, alternate := 0, false
	for index := len(value) - 1; index >= 0; index-- {
		digit := int(value[index] - '0')
		if alternate {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum, alternate = sum+digit, !alternate
	}
	return sum%10 == 0
}
