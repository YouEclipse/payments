// Package rules compiles bounded Router-owned policy artifacts. It performs no
// I/O and cannot add candidates that capability eligibility has removed.
package rules

import (
	"time"

	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
)

const SchemaVersion = "routing_rules.v1"

const (
	True    = "true"
	False   = "false"
	Unknown = "unknown"
)

// Condition is a deliberately small typed AST: conjunction, string equality,
// string membership and inclusive int64 ranges. No expression language, code,
// regex, floating point, wall clock or remote lookup is evaluated.
type Condition struct {
	Op     string      `json:"op"`
	Field  string      `json:"field,omitempty"`
	Value  *string     `json:"value,omitempty"`
	Values []string    `json:"values,omitempty"`
	Min    *int64      `json:"min,omitempty"`
	Max    *int64      `json:"max,omitempty"`
	All    []Condition `json:"all,omitempty"`
}

type Weight struct {
	CandidateID string `json:"candidate_id"`
	Weight      int    `json:"weight"`
}

type Action struct {
	Type         string   `json:"type"`
	CandidateIDs []string `json:"candidate_ids,omitempty"`
	Weights      []Weight `json:"weights,omitempty"`
}

type Rule struct {
	ID         string    `json:"rule_id"`
	Priority   int       `json:"priority"`
	Condition  Condition `json:"condition"`
	Action     Action    `json:"action"`
	ReasonCode string    `json:"reason_code"`
}

type Artifact struct {
	SchemaVersion         string    `json:"schema_version"`
	Version               string    `json:"version"`
	PublishedAt           time.Time `json:"published_at"`
	EffectiveFrom         time.Time `json:"effective_from"`
	ExpiresAt             time.Time `json:"expires_at"`
	DefaultCandidateOrder []string  `json:"default_candidate_order"`
	Rules                 []Rule    `json:"rules"`
}

type Hit struct {
	RuleID       string   `json:"rule_id"`
	Truth        string   `json:"truth"`
	Action       string   `json:"action"`
	ReasonCode   string   `json:"reason_code"`
	CandidateIDs []string `json:"candidate_ids"`
}

type Exclusion struct{ CandidateID, ReasonCode string }

type Result struct {
	CandidateOrder []string
	Hits           []Hit
	ReasonCodes    []string
	Exclusions     []Exclusion
}

type Evaluator struct {
	artifact Artifact
	digest   string
	// Execution order is explicit: descending priority, then stable rule ID.
	ordered []Rule
}

func (e *Evaluator) Version() string { return e.artifact.Version }
func (e *Evaluator) Digest() string  { return e.digest }
func (e *Evaluator) ValidAt(at time.Time) bool {
	return !at.Before(e.artifact.PublishedAt) && !at.Before(e.artifact.EffectiveFrom) && at.Before(e.artifact.ExpiresAt)
}

func truth(condition Condition, context routingcontext.Context) string {
	if condition.Op == "all" {
		result := True
		for _, child := range condition.All {
			switch truth(child, context) {
			case False:
				return False
			case Unknown:
				result = Unknown
			}
		}
		return result
	}
	value := context.Lookup(condition.Field)
	if value.State != routingcontext.StateKnown {
		return Unknown
	}
	match := false
	switch condition.Op {
	case "eq":
		match = value.String == *condition.Value
	case "in":
		for _, option := range condition.Values {
			if value.String == option {
				match = true
				break
			}
		}
	case "range":
		match = value.Integer >= *condition.Min && value.Integer <= *condition.Max
	}
	if match {
		return True
	}
	return False
}
