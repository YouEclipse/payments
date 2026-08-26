// Package decision connects Context, hard eligibility and deterministic rules.
// It owns no payment state and never authorizes a second funds operation.
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/capability"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/rules"
)

type Config struct {
	RuleSet           rules.Artifact      `json:"rule_set"`
	CapabilityCatalog capability.Artifact `json:"capability_catalog"`
}

type Decision struct {
	ID                       string                `json:"decision_id"`
	SelectedCandidate        *routing.CandidateRef `json:"selected_candidate,omitempty"`
	CandidateOrder           []string              `json:"candidate_order"`
	Exclusions               []routing.Exclusion   `json:"exclusions"`
	PolicyVersion            string                `json:"policy_version"`
	ContextSchemaVersion     string                `json:"context_schema_version"`
	ContextDigest            string                `json:"context_digest"`
	RuleSetVersion           string                `json:"rule_set_version"`
	RuleSetDigest            string                `json:"rule_set_digest"`
	CapabilityCatalogVersion string                `json:"capability_catalog_version"`
	CapabilityCatalogDigest  string                `json:"capability_catalog_digest"`
	RuleHits                 []rules.Hit           `json:"rule_hits"`
	Eligibility              []capability.Result   `json:"eligibility"`
	ReasonCodes              []string              `json:"reason_codes"`
}

type Engine struct {
	catalog *capability.Catalog
	rules   *rules.Evaluator
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
	return &Engine{catalog: catalog, rules: evaluator}, nil
}

// ReadyAt is an online admission check, deliberately outside the pure Decide
// function. A process with expired Router-owned snapshots cannot issue a new
// execution decision by accepting an old, still replayable Context.
func (e *Engine) ReadyAt(now time.Time) bool { return e.catalog.ValidAt(now) && e.rules.ValidAt(now) }

func (e *Engine) ExecutableAt(result Decision, now time.Time) bool {
	return e.ReadyAt(now) && e.catalog.CandidatesValidAt(result.CandidateOrder, now)
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
	if len(result.CandidateOrder) > 0 {
		selected := candidates[result.CandidateOrder[0]]
		result.SelectedCandidate = &selected
	} else {
		result.ReasonCodes = append(result.ReasonCodes, "no_eligible_provider")
	}
	// ID has a fixed empty value in the canonical form; the complete evidence
	// includes context and artifact digests, so identity/revision changes cannot
	// accidentally reuse a previous decision identifier.
	canonical, _ := json.Marshal(result)
	hash := sha256.Sum256(canonical)
	result.ID = "rtd_" + hex.EncodeToString(hash[:16])
	if result.SelectedCandidate == nil {
		return result, routing.ErrNoEligibleProvider
	}
	return result, nil
}
