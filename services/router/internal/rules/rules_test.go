package rules_test

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/rules"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

var candidates = []string{"conn_us", "conn_eu"}

func compile(t *testing.T, artifact rules.Artifact) *rules.Evaluator {
	t.Helper()
	engine, err := rules.Compile(artifact, candidates)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestRuleTruthUsesTypedFieldsAndKleeneConjunction(t *testing.T) {
	for _, test := range []struct {
		name      string
		condition rules.Condition
		truth     string
	}{
		{"equality", rules.Condition{Op: "eq", Field: "base.currency", Value: testfixture.String("USD")}, rules.True},
		{"membership", rules.Condition{Op: "in", Field: "base.currency", Values: []string{"EUR", "USD"}}, rules.True},
		{"inclusive range", rules.Condition{Op: "range", Field: "base.amount_minor", Min: testfixture.Int(5200), Max: testfixture.Int(5200)}, rules.True},
		{"range miss", rules.Condition{Op: "range", Field: "base.amount_minor", Min: testfixture.Int(1), Max: testfixture.Int(5199)}, rules.False},
		{"unknown network", rules.Condition{Op: "eq", Field: "payment_method_context.network", Value: testfixture.String("visa")}, rules.Unknown},
		{"false dominates unknown", rules.Condition{Op: "all", All: []rules.Condition{{Op: "eq", Field: "payment_method_context.network", Value: testfixture.String("visa")}, {Op: "eq", Field: "base.currency", Value: testfixture.String("EUR")}}}, rules.False},
		{"true plus unknown", rules.Condition{Op: "all", All: []rules.Condition{{Op: "eq", Field: "payment_method_context.network", Value: testfixture.String("visa")}, {Op: "eq", Field: "base.currency", Value: testfixture.String("USD")}}}, rules.Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := testfixture.RuleSet()
			rule := testfixture.Rule("typed", "prefer", 100, "conn_eu")
			rule.Condition = test.condition
			artifact.Rules = []rules.Rule{rule}
			result, err := compile(t, artifact).Evaluate(testfixture.Context(), candidates)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Hits) != 1 || result.Hits[0].Truth != test.truth {
				t.Fatalf("truth=%+v", result.Hits)
			}
			if test.truth != rules.True && result.CandidateOrder[0] != "conn_us" {
				t.Fatal("unknown or false condition fired")
			}
		})
	}
}

func TestForceExcludeAndSplitCannotResurrectIneligibleCandidate(t *testing.T) {
	for _, action := range []string{"prefer", "force", "split"} {
		t.Run(action, func(t *testing.T) {
			artifact := testfixture.RuleSet()
			rule := testfixture.Rule("target_eu", action, 100, "conn_eu")
			if action == "split" {
				rule.Action = rules.Action{Type: "split", Weights: []rules.Weight{{CandidateID: "conn_eu", Weight: 10000}}}
			}
			artifact.Rules = []rules.Rule{rule}
			result, err := compile(t, artifact).Evaluate(testfixture.Context(), []string{"conn_us"})
			if err != nil {
				t.Fatal(err)
			}
			if action == "prefer" {
				if !reflect.DeepEqual(result.CandidateOrder, []string{"conn_us"}) {
					t.Fatal(result.CandidateOrder)
				}
			} else if len(result.CandidateOrder) != 0 {
				t.Fatal("hard action bypassed eligibility", result.CandidateOrder)
			}
		})
	}
	artifact := testfixture.RuleSet()
	artifact.Rules = []rules.Rule{testfixture.Rule("high_force", "force", 200, "conn_us"), testfixture.Rule("lower_deny", "exclude", 100, "conn_us")}
	result, err := compile(t, artifact).Evaluate(testfixture.Context(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CandidateOrder) != 0 {
		t.Fatal("force bypassed hard exclusion")
	}
	if _, err := compile(t, testfixture.RuleSet()).Evaluate(testfixture.Context(), []string{"not_in_manifest"}); err == nil {
		t.Fatal("unknown candidate injection succeeded")
	}
}

func TestRuleCompileRejectsConflictsAndPartialArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*rules.Artifact)
	}{
		{"schema", func(a *rules.Artifact) { a.SchemaVersion = "routing_rules.v99" }},
		{"omitted rules", func(a *rules.Artifact) { a.Rules = nil }},
		{"partial default", func(a *rules.Artifact) { a.DefaultCandidateOrder = []string{"conn_us"} }},
		{"duplicate default", func(a *rules.Artifact) { a.DefaultCandidateOrder = []string{"conn_us", "conn_us"} }},
		{"duplicate rule", func(a *rules.Artifact) { a.Rules = append(a.Rules, a.Rules[0]) }},
		{"unknown field", func(a *rules.Artifact) {
			a.Rules[0].Condition = rules.Condition{Op: "eq", Field: "payment_method_token", Value: testfixture.String("secret")}
		}},
		{"field type", func(a *rules.Artifact) {
			a.Rules[0].Condition = rules.Condition{Op: "eq", Field: "base.amount_minor", Value: testfixture.String("5200")}
		}},
		{"currency literal casing", func(a *rules.Artifact) {
			a.Rules[0].Condition = rules.Condition{Op: "eq", Field: "base.currency", Value: testfixture.String("usd")}
		}},
		{"network literal enum", func(a *rules.Artifact) {
			a.Rules[0].Condition = rules.Condition{Op: "in", Field: "payment_method_context.network", Values: []string{"visa", "invented"}}
		}},
		{"unknown action", func(a *rules.Artifact) { a.Rules[0].Action.Type = "evaluate_code" }},
		{"missing priority", func(a *rules.Artifact) { a.Rules[0].Priority = 0 }},
		{"absent target", func(a *rules.Artifact) { a.Rules[0].Action.CandidateIDs = []string{"unknown_connection"} }},
		{"ambiguous AST", func(a *rules.Artifact) { a.Rules[0].Condition = rules.Condition{Op: "all", Field: "base.currency"} }},
		{"bad interval", func(a *rules.Artifact) {
			a.Rules[0].Condition = rules.Condition{Op: "range", Field: "base.amount_minor", Min: testfixture.Int(10), Max: testfixture.Int(1)}
		}},
		{"unsatisfiable", func(a *rules.Artifact) {
			a.Rules[0].Condition = rules.Condition{Op: "all", All: []rules.Condition{{Op: "eq", Field: "base.currency", Value: testfixture.String("USD")}, {Op: "eq", Field: "base.currency", Value: testfixture.String("EUR")}}}
		}},
		{"same priority prefer", func(a *rules.Artifact) {
			a.Rules = append(a.Rules, testfixture.Rule("different", "prefer", 100, "conn_eu"))
		}},
		{"same priority force", func(a *rules.Artifact) {
			a.Rules = []rules.Rule{testfixture.Rule("force_us", "force", 100, "conn_us"), testfixture.Rule("force_eu", "force", 100, "conn_eu")}
		}},
		{"force versus exclude", func(a *rules.Artifact) {
			a.Rules = []rules.Rule{testfixture.Rule("force_us", "force", 100, "conn_us"), testfixture.Rule("exclude_us", "exclude", 100, "conn_us")}
		}},
		{"force versus disjoint split", func(a *rules.Artifact) {
			force := testfixture.Rule("force_us", "force", 100, "conn_us")
			split := testfixture.Rule("split_eu", "split", 100)
			split.Action.Weights = []rules.Weight{{CandidateID: "conn_eu", Weight: 10000}}
			a.Rules = []rules.Rule{force, split}
		}},
		{"invalid split total", func(a *rules.Artifact) {
			a.Rules[0].Action = rules.Action{Type: "split", Weights: []rules.Weight{{CandidateID: "conn_us", Weight: 1}}}
		}},
		{"same priority equal splits use different seeds", func(a *rules.Artifact) {
			first, second := testfixture.Rule("split_one", "split", 100), testfixture.Rule("split_two", "split", 100)
			first.Action.Weights = []rules.Weight{{CandidateID: "conn_us", Weight: 7500}, {CandidateID: "conn_eu", Weight: 2500}}
			second.Action.Weights = append([]rules.Weight{}, first.Action.Weights...)
			a.Rules = []rules.Rule{first, second}
		}},
		{"duplicate split candidate", func(a *rules.Artifact) {
			a.Rules[0].Action = rules.Action{Type: "split", Weights: []rules.Weight{{CandidateID: "conn_us", Weight: 5000}, {CandidateID: "conn_us", Weight: 5000}}}
		}},
		{"AST budget", func(a *rules.Artifact) {
			leaf := rules.Condition{Op: "all"}
			for i := 0; i < 10; i++ {
				leaf = rules.Condition{Op: "all", All: []rules.Condition{leaf}}
			}
			a.Rules[0].Condition = leaf
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := testfixture.RuleSet()
			artifact.Rules = []rules.Rule{testfixture.Rule("prefer_us", "prefer", 100, "conn_us")}
			test.change(&artifact)
			if _, err := rules.Compile(artifact, candidates); err == nil {
				t.Fatal("accepted invalid rule artifact")
			}
		})
	}
}

func TestRuleCompileDetectsRangeBoundaryOverlapButAllowsDisjointRules(t *testing.T) {
	artifact := testfixture.RuleSet()
	a, b := testfixture.Rule("small", "force", 100, "conn_us"), testfixture.Rule("large", "force", 100, "conn_eu")
	a.Condition = rules.Condition{Op: "range", Field: "base.amount_minor", Min: testfixture.Int(1), Max: testfixture.Int(5200)}
	b.Condition = rules.Condition{Op: "range", Field: "base.amount_minor", Min: testfixture.Int(5200), Max: testfixture.Int(10000)}
	artifact.Rules = []rules.Rule{a, b}
	if _, err := rules.Compile(artifact, candidates); err == nil {
		t.Fatal("inclusive boundary conflict was not detected")
	}
	artifact.Rules[1].Condition.Min = testfixture.Int(5201)
	result, err := compile(t, artifact).Evaluate(testfixture.Context(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.CandidateOrder, []string{"conn_us"}) {
		t.Fatal(result.CandidateOrder)
	}
}

func TestRuleCanonicalArtifactsImmutableAndOrderIndependent(t *testing.T) {
	artifact := testfixture.RuleSet()
	a, b := testfixture.Rule("usd", "prefer", 100, "conn_us"), testfixture.Rule("eur", "prefer", 100, "conn_eu")
	a.Condition = rules.Condition{Op: "all", All: []rules.Condition{{Op: "in", Field: "base.currency", Values: []string{"USD", "BRL"}}, {Op: "range", Field: "base.amount_minor", Min: testfixture.Int(1), Max: testfixture.Int(99999)}}}
	b.Condition = rules.Condition{Op: "eq", Field: "base.currency", Value: testfixture.String("EUR")}
	artifact.Rules = []rules.Rule{a, b}
	evaluator := compile(t, artifact)
	artifact.Rules[0].Condition.All[0].Values = []string{"BRL", "USD"}
	artifact.Rules[0].Condition.All[0], artifact.Rules[0].Condition.All[1] = artifact.Rules[0].Condition.All[1], artifact.Rules[0].Condition.All[0]
	artifact.Rules[0], artifact.Rules[1] = artifact.Rules[1], artifact.Rules[0]
	reordered := compile(t, artifact)
	if reordered.Digest() != evaluator.Digest() {
		t.Fatal("rules or set order changed semantic digest")
	}
	artifact.DefaultCandidateOrder[0] = "corrupt"
	artifact.Rules[1].Action.CandidateIDs[0] = "corrupt"
	baseline, err := evaluator.Evaluate(testfixture.Context(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := evaluator.Evaluate(testfixture.Context(), candidates)
			if err != nil || !reflect.DeepEqual(baseline, result) {
				t.Error("immutable concurrent evaluation drifted", err)
			}
		}()
	}
	group.Wait()
	baseline.Hits[0].CandidateIDs[0] = "corrupt"
	fresh, err := evaluator.Evaluate(testfixture.Context(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Hits[0].CandidateIDs[0] == "corrupt" {
		t.Fatal("result aliases compiled action")
	}
}

func TestSplitStableAssignmentOnlyWithinEligibleWeightedSet(t *testing.T) {
	artifact := testfixture.RuleSet()
	split := testfixture.Rule("split", "split", 100)
	split.Action.Weights = []rules.Weight{{CandidateID: "conn_us", Weight: 7500}, {CandidateID: "conn_eu", Weight: 2500}}
	artifact.Rules = []rules.Rule{split}
	evaluator := compile(t, artifact)
	context := testfixture.Context()
	first, err := evaluator.Evaluate(context, candidates)
	if err != nil {
		t.Fatal(err)
	}
	context.Base.AttemptID = "att_another"
	other, err := evaluator.Evaluate(context, []string{"conn_eu", "conn_us"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.CandidateOrder, other.CandidateOrder) {
		t.Fatal("attempt or eligible input order reassigned split")
	}
	artifact.Rules[0].Action.Weights[0], artifact.Rules[0].Action.Weights[1] = artifact.Rules[0].Action.Weights[1], artifact.Rules[0].Action.Weights[0]
	reordered := compile(t, artifact)
	if reordered.Digest() != evaluator.Digest() {
		t.Fatal("split set order changed digest")
	}
	for _, id := range candidates {
		result, err := evaluator.Evaluate(context, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.CandidateOrder, []string{id}) {
			t.Fatal("split introduced ineligible connection")
		}
	}
	empty, err := evaluator.Evaluate(context, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.CandidateOrder) != 0 {
		t.Fatal("split manufactured a fallback")
	}
	context.BuiltAt = context.BuiltAt.Add(time.Hour)
	context.Merchant.Country = routingcontext.Known("US", "config", "v1", context.BuiltAt, context.BuiltAt.Add(time.Hour))
	context.PaymentMethod.CredentialDomain = routingcontext.Known("simulator_a", "vault", "v1", context.BuiltAt, context.BuiltAt.Add(time.Hour))
	expired, err := evaluator.Evaluate(context, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired.CandidateOrder) != 0 || expired.ReasonCodes[0] != "rule_set_not_effective" {
		t.Fatal("expired rules executed")
	}
}
