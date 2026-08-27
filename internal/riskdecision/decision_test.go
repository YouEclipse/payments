package riskdecision

import (
	"strings"
	"testing"
	"time"
)

func fixture() (time.Time, Decision) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	return now, Decision{SchemaVersion: SchemaVersion, DecisionID: "risk_test", SubjectDigest: "sha256:" + strings.Repeat("a", 64),
		Action: Allow, ReasonCodes: []string{}, Version: "policy_v1", Source: "fixture", EvaluatedAt: now, ExpiresAt: now.Add(time.Minute)}
}

func TestActionsCannotBeCompensatedByScore(t *testing.T) {
	for _, action := range []string{Allow, Block, Require3DS, ManualReview} {
		now, decision := fixture()
		decision.Action, decision.ReasonCodes = action, []string{"test_action"}
		result, err := EvaluateSale(now, decision.SubjectDigest, decision)
		if err != nil || result.Continue != (action == Allow) || !ValidDigest(result.DecisionDigest) {
			t.Fatalf("%s: %+v %v", action, result, err)
		}
		result.ReasonCodes[0] = "changed"
		if decision.ReasonCodes[0] != "test_action" {
			t.Fatal("evidence aliases source")
		}
	}
}

func TestRiskIdentityFreshnessAndReasonsFailClosed(t *testing.T) {
	for _, name := range []string{"subject", "expires", "future", "missing", "unknown_action", "unsorted", "duplicate", "free_text", "no_reason"} {
		t.Run(name, func(t *testing.T) {
			now, decision := fixture()
			subject := decision.SubjectDigest
			switch name {
			case "subject":
				decision.SubjectDigest = "sha256:" + strings.Repeat("b", 64)
			case "expires":
				decision.ExpiresAt = now
			case "future":
				decision.EvaluatedAt = now.Add(time.Second)
			case "missing":
				decision.ReasonCodes = nil
			case "unknown_action":
				decision.Action = "risk_low"
			case "unsorted":
				decision.ReasonCodes = []string{"z", "a"}
			case "duplicate":
				decision.ReasonCodes = []string{"a", "a"}
			case "free_text":
				decision.ReasonCodes = []string{"customer@example.invalid"}
			case "no_reason":
				decision.Action = Block
			}
			if _, err := EvaluateSale(now, subject, decision); err == nil {
				t.Fatal("invalid risk decision accepted")
			}
		})
	}
}
