// Package decision connects Context, hard eligibility and deterministic rules.
// It owns no payment state and never authorizes a second funds operation.
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/router/internal/capability"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/rules"
)

type Config struct {
	RuleSet           rules.Artifact      `json:"rule_set"`
	CapabilityCatalog capability.Artifact `json:"capability_catalog"`
	Optimization      *OptimizationConfig `json:"optimization,omitempty"`
}

type Decision struct {
	ID                       string                 `json:"decision_id"`
	SelectedCandidate        *routing.CandidateRef  `json:"selected_candidate,omitempty"`
	CandidateOrder           []string               `json:"candidate_order"`
	Exclusions               []routing.Exclusion    `json:"exclusions"`
	PolicyVersion            string                 `json:"policy_version"`
	ContextSchemaVersion     string                 `json:"context_schema_version"`
	ContextDigest            string                 `json:"context_digest"`
	ExecutableUntil          time.Time              `json:"executable_until"`
	RuleSetVersion           string                 `json:"rule_set_version"`
	RuleSetDigest            string                 `json:"rule_set_digest"`
	CapabilityCatalogVersion string                 `json:"capability_catalog_version"`
	CapabilityCatalogDigest  string                 `json:"capability_catalog_digest"`
	RuleHits                 []rules.Hit            `json:"rule_hits"`
	Eligibility              []capability.Result    `json:"eligibility"`
	Risk                     *riskdecision.Evidence `json:"risk,omitempty"`
	Optimization             *OptimizationResult    `json:"optimization,omitempty"`
	ReasonCodes              []string               `json:"reason_codes"`
	optimizationValidity     *optimizationValidity  `json:"-"`
}

type Engine struct {
	catalog      *capability.Catalog
	rules        *rules.Evaluator
	optimization *compiledOptimization
}

func NewEngine(config Config) (*Engine, error) {
	catalog, err := capability.Compile(config.CapabilityCatalog)
	if err != nil {
		return nil, fmt.Errorf("compile capability catalog: %w", err)
	}
	evaluator, err := rules.Compile(config.RuleSet, catalog.CandidateIDs())
	if err != nil {
		return nil, fmt.Errorf("compile rule set: %w", err)
	}
	engine := &Engine{catalog: catalog, rules: evaluator}
	if config.Optimization != nil {
		optimization, err := compileOptimization(*config.Optimization, catalog.Candidates())
		if err != nil {
			return nil, err
		}
		engine.optimization = optimization
	}
	return engine, nil
}

func (e *Engine) OptimizationConfigured() bool { return e != nil && e.optimization != nil }

// ReadyAt is an online admission check, deliberately outside the pure Decide
// function. A process with expired Router-owned snapshots cannot issue a new
// execution decision by accepting an old, still replayable Context.
func (e *Engine) ReadyAt(now time.Time) bool {
	return e != nil && e.catalog.ValidAt(now) && e.rules.ValidAt(now) && (e.optimization == nil || e.optimization.readyAt(now))
}

// AuthoritativeAt checks whether a returned decision (including a rejection)
// still carries current, content-bound evidence. It does not turn a rejected
// decision into execution permission.
func (e *Engine) AuthoritativeAt(result Decision, now time.Time) bool {
	if !e.ReadyAt(now) || !e.catalog.CandidatesValidAt(result.CandidateOrder, now) ||
		result.ExecutableUntil.IsZero() || !now.Before(result.ExecutableUntil) || !validDecisionID(result) {
		return false
	}
	if result.Optimization != nil {
		return e.optimization != nil && result.optimizationValidity != nil &&
			result.optimizationValidity.configDigest == e.optimization.digest &&
			now.Before(result.optimizationValidity.validUntil)
	}
	return true
}

func (e *Engine) ExecutableAt(result Decision, now time.Time) bool {
	if !e.AuthoritativeAt(result, now) {
		return false
	}
	if e.optimization == nil {
		return true
	}
	return result.Risk != nil && result.Risk.Continue && result.Optimization != nil &&
		result.Optimization.ConfigVersion == e.optimization.version && result.Optimization.ConfigDigest == e.optimization.digest &&
		result.optimizationValidity != nil
}

// Decide evaluates the frozen Context at BuiltAt. The same semantic artifacts
// and Context always produce identical evidence, including the decision ID.
func (e *Engine) Decide(context routingcontext.Context) (Decision, error) {
	contextDigest, err := context.Digest()
	if err != nil {
		return Decision{}, err
	}
	result := Decision{
		CandidateOrder: []string{}, Exclusions: []routing.Exclusion{},
		PolicyVersion: e.rules.Version(), ContextSchemaVersion: context.SchemaVersion, ContextDigest: contextDigest,
		RuleSetVersion: e.rules.Version(), RuleSetDigest: e.rules.Digest(),
		CapabilityCatalogVersion: e.catalog.Version(), CapabilityCatalogDigest: e.catalog.Digest(),
		RuleHits: []rules.Hit{}, Eligibility: e.catalog.Filter(context), ReasonCodes: []string{},
	}
	candidates := make(map[string]routing.CandidateRef)
	for _, candidate := range e.catalog.Candidates() {
		candidates[candidate.ID] = candidate
	}
	eligible := []string{}
	exclusions := make(map[string][]routing.Reason)
	for _, item := range result.Eligibility {
		if item.Status == capability.Eligible {
			eligible = append(eligible, item.CandidateID)
		} else {
			exclusions[item.CandidateID] = append([]routing.Reason{}, item.Reasons...)
		}
	}
	ruleResult, err := e.rules.Evaluate(context, eligible)
	if err != nil {
		return Decision{}, err
	}
	result.CandidateOrder, result.RuleHits, result.ReasonCodes = ruleResult.CandidateOrder, ruleResult.Hits, ruleResult.ReasonCodes
	for _, exclusion := range ruleResult.Exclusions {
		exclusions[exclusion.CandidateID] = append(exclusions[exclusion.CandidateID], routing.Reason{Code: exclusion.ReasonCode, Detail: "candidate is excluded by a matching hard rule constraint"})
	}
	for candidateID, reasons := range exclusions {
		result.Exclusions = append(result.Exclusions, routing.Exclusion{CandidateID: candidateID, Provider: candidates[candidateID].Provider, Reasons: reasons})
	}
	sort.Slice(result.Exclusions, func(i, j int) bool { return result.Exclusions[i].CandidateID < result.Exclusions[j].CandidateID })
	if e.optimization != nil {
		if context.SchemaVersion != routingcontext.RiskSchemaVersion || context.Risk == nil || context.Base.Operation != "sale" || context.Base.CaptureMethod != "automatic" {
			return Decision{}, fmt.Errorf("%w: optimization requires routing_context.v2 sale/automatic", ErrInvalidOptimization)
		}
		subjectDigest, err := context.RiskSubjectDigest()
		if err != nil {
			return Decision{}, err
		}
		risk, err := riskdecision.EvaluateSale(context.BuiltAt, subjectDigest, *context.Risk)
		if err != nil {
			return Decision{}, err
		}
		result.Risk = &risk
		if !risk.Continue {
			result.CandidateOrder = []string{}
			result.ReasonCodes = appendUnique(result.ReasonCodes, "risk_"+risk.Outcome)
		} else if len(result.CandidateOrder) > 0 {
			optimization, validity, err := e.optimization.evaluate(context, candidates, result.CandidateOrder)
			if err != nil {
				return Decision{}, err
			}
			result.Optimization, result.optimizationValidity = &optimization, validity
			result.CandidateOrder = append([]string(nil), optimization.Scoring.CandidateOrder...)
			result.ReasonCodes = appendUnique(result.ReasonCodes, optimization.Scoring.ReasonCodes...)
		}
	}
	if err := finalizeDecision(&result, candidates); err != nil {
		return Decision{}, err
	}
	executableUntil, err := e.executionAuthorityUntil(context, result)
	if err != nil {
		return Decision{}, err
	}
	result.ExecutableUntil = executableUntil
	if err := identifyDecision(&result); err != nil {
		return Decision{}, err
	}
	if result.SelectedCandidate == nil {
		return result, routing.ErrNoEligibleProvider
	}
	return result, nil
}

func finalizeDecision(result *Decision, candidates map[string]routing.CandidateRef) error {
	result.SelectedCandidate = nil
	if len(result.CandidateOrder) > 0 {
		selected, exists := candidates[result.CandidateOrder[0]]
		if !exists {
			return fmt.Errorf("candidate order contains unknown identity %q", result.CandidateOrder[0])
		}
		result.SelectedCandidate = &selected
	} else {
		result.ReasonCodes = appendUnique(result.ReasonCodes, "no_eligible_provider")
	}
	return nil
}

func (e *Engine) executionAuthorityUntil(context routingcontext.Context, result Decision) (time.Time, error) {
	until, err := context.OnlineAuthorityUntil(routingcontext.OnlineMaxAge)
	if err != nil {
		return time.Time{}, err
	}
	if expiry := e.rules.ExpiresAt(); expiry.Before(until) {
		until = expiry
	}
	catalogUntil, ok := e.catalog.CandidatesAuthorityUntil(result.CandidateOrder)
	if !ok {
		return time.Time{}, errors.New("candidate order is absent from capability authority")
	}
	if catalogUntil.Before(until) {
		until = catalogUntil
	}
	if result.optimizationValidity != nil && result.optimizationValidity.validUntil.Before(until) {
		until = result.optimizationValidity.validUntil
	}
	// PostgreSQL timestamptz and the durable relational deadline use
	// microsecond precision. Truncation is deliberate: serialization may shorten
	// authority, but it must never round a Router deadline forward.
	return until.UTC().Truncate(time.Microsecond), nil
}

func identifyDecision(result *Decision) error {
	result.ID = ""
	canonical, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode canonical routing decision: %w", err)
	}
	hash := sha256.Sum256(canonical)
	result.ID = "rtd_" + hex.EncodeToString(hash[:16])
	return nil
}

func validDecisionID(result Decision) bool {
	want := result.ID
	result.ID = ""
	canonical, err := json.Marshal(result)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(canonical)
	return want == "rtd_"+hex.EncodeToString(hash[:16])
}

func appendUnique(values []string, additions ...string) []string {
	for _, addition := range additions {
		seen := false
		for _, existing := range values {
			seen = seen || existing == addition
		}
		if !seen {
			values = append(values, addition)
		}
	}
	return values
}
