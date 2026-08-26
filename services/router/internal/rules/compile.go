package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
)

var artifactID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func Compile(input Artifact, candidateIDs []string) (*Evaluator, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, errors.New("invalid rule artifact")
	}
	var artifact Artifact
	if json.Unmarshal(encoded, &artifact) != nil {
		return nil, errors.New("invalid rule artifact")
	}
	if artifact.SchemaVersion != SchemaVersion || !routingcontext.ValidIdentifier(artifact.Version) ||
		!validInterval(artifact.PublishedAt, artifact.EffectiveFrom, artifact.ExpiresAt) || artifact.Rules == nil || len(artifact.Rules) > 256 {
		return nil, errors.New("rule set requires a supported version, bounded rules and a valid effective interval")
	}
	manifest := make(map[string]bool, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		if !artifactID.MatchString(candidateID) || manifest[candidateID] {
			return nil, errors.New("invalid candidate manifest")
		}
		manifest[candidateID] = true
	}
	if len(manifest) == 0 || len(manifest) > 128 || len(artifact.DefaultCandidateOrder) != len(manifest) {
		return nil, errors.New("default order must cover the complete candidate manifest")
	}
	if err := validateIDs(artifact.DefaultCandidateOrder, manifest); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for index := range artifact.Rules {
		rule := &artifact.Rules[index]
		if !artifactID.MatchString(rule.ID) || seen[rule.ID] || !artifactID.MatchString(rule.ReasonCode) || rule.Priority < 1 || rule.Priority > 10000 {
			return nil, errors.New("rule identity, reason code or priority is invalid")
		}
		seen[rule.ID] = true
		budget := 64
		if err := normalizeCondition(&rule.Condition, 0, &budget); err != nil {
			return nil, err
		}
		if _, possible := domains(rule.Condition); !possible {
			return nil, errors.New("rule condition is unsatisfiable")
		}
		if err := normalizeAction(&rule.Action, manifest); err != nil {
			return nil, err
		}
	}
	// Compilation rejects overlapping, order-sensitive same-priority actions.
	// A stable ID is a serialization tiebreaker, never a hidden conflict policy.
	for i := range artifact.Rules {
		for j := i + 1; j < len(artifact.Rules); j++ {
			a, b := artifact.Rules[i], artifact.Rules[j]
			if a.Priority == b.Priority && overlap(a.Condition, b.Condition) && incompatible(a.Action, b.Action) {
				return nil, fmt.Errorf("same-priority rule conflict: %s and %s", a.ID, b.ID)
			}
		}
	}
	sort.Slice(artifact.Rules, func(i, j int) bool { return artifact.Rules[i].ID < artifact.Rules[j].ID })
	encoded, _ = json.Marshal(artifact)
	hash := sha256.Sum256(encoded)
	ordered := append([]Rule(nil), artifact.Rules...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority > ordered[j].Priority
		}
		return ordered[i].ID < ordered[j].ID
	})
	return &Evaluator{artifact: artifact, digest: "sha256:" + hex.EncodeToString(hash[:]), ordered: ordered}, nil
}

func normalizeCondition(condition *Condition, depth int, budget *int) error {
	*budget -= 1
	if depth > 8 || *budget < 0 {
		return errors.New("rule AST exceeds its bounded complexity")
	}
	if condition.Op == "all" {
		if condition.Field != "" || condition.Value != nil || len(condition.Values) != 0 || condition.Min != nil || condition.Max != nil || len(condition.All) > 32 {
			return errors.New("all accepts only a bounded list of child conditions")
		}
		flattened := []Condition{}
		for index := range condition.All {
			child := &condition.All[index]
			if err := normalizeCondition(child, depth+1, budget); err != nil {
				return err
			}
			if child.Op == "all" {
				flattened = append(flattened, child.All...)
			} else {
				flattened = append(flattened, *child)
			}
		}
		sort.Slice(flattened, func(i, j int) bool { return conditionKey(flattened[i]) < conditionKey(flattened[j]) })
		condition.All = flattened
		return nil
	}
	if len(condition.All) != 0 {
		return errors.New("leaf condition cannot contain child conditions")
	}
	kind, exists := routingcontext.FieldKind(condition.Field)
	if !exists {
		return errors.New("condition field is not in this Context schema")
	}
	switch condition.Op {
	case "eq":
		if kind != routingcontext.KindString || condition.Value == nil || !routingcontext.ValidFieldString(condition.Field, *condition.Value) ||
			len(condition.Values) != 0 || condition.Min != nil || condition.Max != nil {
			return errors.New("eq requires one typed string value")
		}
	case "in":
		if kind != routingcontext.KindString || condition.Value != nil || len(condition.Values) == 0 || len(condition.Values) > 128 ||
			condition.Min != nil || condition.Max != nil {
			return errors.New("in requires a bounded nonempty string set")
		}
		seen := make(map[string]bool)
		for _, value := range condition.Values {
			if !routingcontext.ValidFieldString(condition.Field, value) || seen[value] {
				return errors.New("in contains an invalid or duplicate value")
			}
			seen[value] = true
		}
		sort.Strings(condition.Values)
	case "range":
		if kind != routingcontext.KindInteger || condition.Value != nil || len(condition.Values) != 0 ||
			condition.Min == nil || condition.Max == nil || *condition.Min < 1 || *condition.Min > *condition.Max {
			return errors.New("range requires an inclusive positive int64 interval")
		}
	default:
		return errors.New("unsupported rule operator")
	}
	return nil
}

func normalizeAction(action *Action, candidates map[string]bool) error {
	switch action.Type {
	case "prefer", "exclude", "force":
		if len(action.Weights) != 0 || len(action.CandidateIDs) == 0 || (action.Type == "force" && len(action.CandidateIDs) != 1) {
			return errors.New("prefer/exclude require candidates; force requires exactly one candidate")
		}
		if err := validateIDs(action.CandidateIDs, candidates); err != nil {
			return err
		}
		if action.Type == "exclude" {
			sort.Strings(action.CandidateIDs)
		}
	case "split":
		if len(action.CandidateIDs) != 0 || len(action.Weights) == 0 || len(action.Weights) > len(candidates) {
			return errors.New("split requires a nonempty weighted candidate set")
		}
		seen := make(map[string]bool)
		total := 0
		for _, weight := range action.Weights {
			if !candidates[weight.CandidateID] || seen[weight.CandidateID] || weight.Weight < 1 || weight.Weight > 10000 {
				return errors.New("invalid split candidate or positive basis-point weight")
			}
			seen[weight.CandidateID] = true
			total += weight.Weight
		}
		if total != 10000 {
			return errors.New("split weights must total 10000 basis points")
		}
		sort.Slice(action.Weights, func(i, j int) bool { return action.Weights[i].CandidateID < action.Weights[j].CandidateID })
	default:
		return errors.New("unsupported rule action")
	}
	return nil
}

func validateIDs(ids []string, candidates map[string]bool) error {
	seen := make(map[string]bool)
	for _, id := range ids {
		if !candidates[id] || seen[id] {
			return errors.New("rule order or action references an absent or duplicate candidate")
		}
		seen[id] = true
	}
	return nil
}

func conditionKey(condition Condition) string {
	encoded, _ := json.Marshal(condition)
	return string(encoded)
}

func validInterval(published, effective, expires time.Time) bool {
	for _, instant := range []time.Time{published, effective, expires} {
		_, offset := instant.Zone()
		if instant.IsZero() || offset != 0 {
			return false
		}
	}
	return expires.After(published) && expires.After(effective)
}

type domain struct {
	kind     string
	values   map[string]bool
	min, max int64
}

// The restricted conjunction grammar makes overlap detection decidable. A
// missing constraint denotes ANY for compiler analysis only, never eligibility.
func domains(condition Condition) (map[string]domain, bool) {
	result := make(map[string]domain)
	leaves := []Condition{condition}
	if condition.Op == "all" {
		leaves = condition.All
	}
	for _, leaf := range leaves {
		current := domain{kind: routingcontext.KindString, values: make(map[string]bool)}
		switch leaf.Op {
		case "eq":
			current.values[*leaf.Value] = true
		case "in":
			for _, value := range leaf.Values {
				current.values[value] = true
			}
		case "range":
			current.kind, current.min, current.max = routingcontext.KindInteger, *leaf.Min, *leaf.Max
		}
		if previous, exists := result[leaf.Field]; exists {
			current = intersection(previous, current)
		}
		if emptyDomain(current) {
			return nil, false
		}
		result[leaf.Field] = current
	}
	return result, true
}

func intersection(a, b domain) domain {
	if a.kind == routingcontext.KindInteger {
		if b.min > a.min {
			a.min = b.min
		}
		if b.max < a.max {
			a.max = b.max
		}
		return a
	}
	values := make(map[string]bool)
	for value := range a.values {
		if b.values[value] {
			values[value] = true
		}
	}
	a.values = values
	return a
}
func emptyDomain(value domain) bool {
	if value.kind == routingcontext.KindInteger {
		return value.min > value.max
	}
	return len(value.values) == 0
}
func overlap(a, b Condition) bool {
	left, _ := domains(a)
	right, _ := domains(b)
	for field, value := range left {
		if other, exists := right[field]; exists && emptyDomain(intersection(value, other)) {
			return false
		}
	}
	return true
}

func incompatible(a, b Action) bool {
	// Exclusions commute. A forced candidate and a matching exclusion are an
	// explicit same-priority contradiction; different priorities fail closed at
	// runtime because exclusions always remain hard constraints.
	if a.Type == "exclude" || b.Type == "exclude" {
		if b.Type == "exclude" {
			a, b = b, a
		}
		if b.Type != "force" {
			return false
		}
		for _, id := range a.CandidateIDs {
			if id == b.CandidateIDs[0] {
				return true
			}
		}
		return false
	}
	if a.Type == "force" || b.Type == "force" {
		if b.Type == "force" {
			a, b = b, a
		}
		if b.Type == "force" {
			return a.CandidateIDs[0] != b.CandidateIDs[0]
		}
		if b.Type == "split" {
			for _, weight := range b.Weights {
				if weight.CandidateID == a.CandidateIDs[0] {
					return false
				}
			}
			return true
		}
		return false
	}
	// A split's rule ID is part of its assignment seed. Equal weight maps in
	// two different rules are therefore not equivalent ordering actions; a
	// same-priority overlap must not be resolved by the serialization ID.
	if a.Type == "split" && b.Type == "split" {
		return true
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) != string(right)
}
