package rules

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
)

// Evaluate consumes only the already eligible candidate IDs. All matching
// excludes apply; the highest-priority matching force narrows the set. The
// highest-priority matching prefer/split orders that remaining set. A split
// narrows to its eligible weighted members; an empty set is a hard no-candidate
// result, never a retry/failover instruction. Lower-priority order actions do
// not silently rewrite a higher-priority assignment.
func (e *Evaluator) Evaluate(context routingcontext.Context, eligible []string) (Result, error) {
	result := Result{CandidateOrder: []string{}, Hits: []Hit{}, ReasonCodes: []string{}, Exclusions: []Exclusion{}}
	if err := context.Validate(); err != nil {
		return result, err
	}
	manifest := make(map[string]bool, len(e.artifact.DefaultCandidateOrder))
	for _, id := range e.artifact.DefaultCandidateOrder {
		manifest[id] = true
	}
	if err := validateIDs(eligible, manifest); err != nil {
		return result, errors.New("eligibility supplied an invalid candidate set")
	}
	if !e.ValidAt(context.BuiltAt) {
		result.ReasonCodes = append(result.ReasonCodes, "rule_set_not_effective")
		return result, nil
	}
	remaining := make(map[string]bool, len(eligible))
	for _, id := range eligible {
		remaining[id] = true
	}
	var force, order *Rule
	reasonSeen := make(map[string]bool)
	addReason := func(code string) {
		if !reasonSeen[code] {
			reasonSeen[code] = true
			result.ReasonCodes = append(result.ReasonCodes, code)
		}
	}
	remove := func(id, code string) {
		if remaining[id] {
			delete(remaining, id)
			result.Exclusions = append(result.Exclusions, Exclusion{CandidateID: id, ReasonCode: code})
		}
	}
	for index := range e.ordered {
		rule := &e.ordered[index]
		matched := truth(rule.Condition, context)
		result.Hits = append(result.Hits, Hit{RuleID: rule.ID, Truth: matched, Action: rule.Action.Type, ReasonCode: rule.ReasonCode, CandidateIDs: actionIDs(rule.Action)})
		if matched == Unknown {
			addReason("rule_condition_unknown")
		}
		if matched != True {
			continue
		}
		addReason(rule.ReasonCode)
		switch rule.Action.Type {
		case "exclude":
			for _, id := range rule.Action.CandidateIDs {
				remove(id, rule.ReasonCode)
			}
		case "force":
			if force == nil {
				force = rule
			}
		case "prefer", "split":
			if order == nil {
				order = rule
			}
		}
	}
	if force != nil {
		forced := force.Action.CandidateIDs[0]
		for _, id := range e.artifact.DefaultCandidateOrder {
			if id != forced {
				remove(id, "rule_force_restricted")
			}
		}
		addReason("rule_force_applied")
	}
	if order != nil && order.Action.Type == "split" {
		members := make(map[string]bool)
		total := uint64(0)
		for _, weight := range order.Action.Weights {
			members[weight.CandidateID] = true
			if remaining[weight.CandidateID] {
				total += uint64(weight.Weight)
			}
		}
		for _, id := range e.artifact.DefaultCandidateOrder {
			if !members[id] {
				remove(id, "rule_split_restricted")
			}
		}
		if total > 0 {
			// Stable across process restarts, request attempts and config file
			// order. The version/rule/payment key is explicit, not math/rand.
			key, _ := json.Marshal(struct {
				Version   string `json:"version"`
				RuleID    string `json:"rule_id"`
				PaymentID string `json:"payment_id"`
			}{e.artifact.Version, order.ID, context.Base.PaymentID})
			digest := sha256.Sum256(key)
			bucket := binary.BigEndian.Uint64(digest[:8]) % total
			for _, weight := range order.Action.Weights {
				if !remaining[weight.CandidateID] {
					continue
				}
				if bucket < uint64(weight.Weight) {
					result.CandidateOrder = append(result.CandidateOrder, weight.CandidateID)
					break
				}
				bucket -= uint64(weight.Weight)
			}
		}
		addReason("rule_split_applied")
	} else if order != nil {
		for _, id := range order.Action.CandidateIDs {
			if remaining[id] {
				result.CandidateOrder = append(result.CandidateOrder, id)
			}
		}
		addReason("rule_prefer_applied")
	} else {
		addReason("rule_default_order")
	}
	appended := make(map[string]bool)
	for _, id := range result.CandidateOrder {
		appended[id] = true
	}
	for _, id := range e.artifact.DefaultCandidateOrder {
		if remaining[id] && !appended[id] {
			result.CandidateOrder = append(result.CandidateOrder, id)
		}
	}
	if len(result.CandidateOrder) == 0 {
		addReason("no_candidate_after_rules")
	}
	return result, nil
}

func actionIDs(action Action) []string {
	if action.Type != "split" {
		return append([]string{}, action.CandidateIDs...)
	}
	ids := make([]string, 0, len(action.Weights))
	for _, weight := range action.Weights {
		ids = append(ids, weight.CandidateID)
	}
	return ids
}
