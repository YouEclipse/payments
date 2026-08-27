package recovery

import (
	"reflect"
	"testing"
	"time"
)

func validState(now time.Time) State {
	first := now.Add(-time.Minute)
	return State{
		Identity: Identity{
			OperationID: "op_1", AttemptID: "att_1", ConnectionID: "simulator_us",
			ConnectionRevision: "v1", NamespaceID: "simulator_account", Provider: "simulator",
			Key: "op_1", PayloadHash: "sha256_payload",
		},
		Status: "unknown", Certainty: "possible", PolicyVersion: PolicyVersion,
		FirstDispatchedAt: &first, DeadlineAt: now.Add(time.Hour), BudgetRemaining: InitialBudget,
	}
}

func TestRecoveryActionTable(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*State)
		want   Action
		review bool
	}{
		{"unknown queries", func(*State) {}, Query, false},
		{"stale sending queries", func(s *State) { s.Status = "sending" }, Query, false},
		{"pending queries", func(s *State) { s.Status = "pending" }, Query, false},
		{"retry hint alone cannot authorize resubmit", func(s *State) { s.Status = "retry_scheduled"; s.Certainty = "none" }, Query, false},
		{"created proves no dispatch", func(s *State) { s.Status = "created"; s.Certainty = "none"; s.FirstDispatchedAt = nil }, SameKeyResubmit, false},
		{"created with possible effects queries", func(s *State) { s.Status = "created" }, Query, false},
		{"legacy missing timestamp cannot prove absence", func(s *State) { s.FirstDispatchedAt = nil }, Query, false},
		{"deadline boundary reviews", func(s *State) { s.DeadlineAt = now }, Stop, true},
		{"zero budget reviews", func(s *State) { s.BudgetRemaining = 0 }, Stop, true},
		{"invalid budget reviews", func(s *State) { s.BudgetRemaining = -1 }, Stop, true},
		{"unknown policy reviews", func(s *State) { s.PolicyVersion = "newer_policy" }, Stop, true},
		{"missing identity reviews", func(s *State) { s.Identity.NamespaceID = "" }, Stop, true},
		{"success stops without review", func(s *State) { s.Status = "succeeded" }, Stop, false},
		{"conclusive failure stops without review", func(s *State) { s.Status = "failed" }, Stop, false},
		{"review is sticky", func(s *State) { s.Status = "review_required" }, Stop, false},
		{"unknown phase reviews", func(s *State) { s.Status = "captured_maybe" }, Stop, true},
		{"backoff waits", func(s *State) { at := now.Add(time.Second); s.NextRetryAt = &at }, Wait, false},
		{"last reserved call waits", func(s *State) { s.BudgetRemaining = 0; at := now.Add(time.Second); s.LeaseUntil = &at }, Wait, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := validState(now)
			test.mutate(&state)
			before := state
			plan := Decide(state, now, time.Second)
			if plan.Action != test.want || plan.RequiresReview != test.review || plan.Reason == "" {
				t.Fatalf("plan=%+v, want %s review=%t", plan, test.want, test.review)
			}
			if plan.ConsumesBudget() != (test.want == Query || test.want == SameKeyResubmit) {
				t.Fatalf("incorrect budget classification: %+v", plan)
			}
			if !reflect.DeepEqual(before, state) {
				t.Fatal("planner mutated durable state")
			}
		})
	}
}

func TestAbsenceEvidenceRequiresWholeIdentityAndHalfOpenValidity(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	for _, kind := range []AbsenceKind{GatewayOperationAbsent, GatewayNeverDispatched, ProviderConclusiveAbsent} {
		state := validState(now)
		state.Absence = &AbsenceEvidence{Kind: kind, Identity: state.Identity, ObservedAt: now, ValidUntil: now.Add(time.Second)}
		wantBasis := ClaimFirstGatewayIO
		if kind == ProviderConclusiveAbsent {
			wantBasis = ClaimProviderRecovery
		}
		if got := Decide(state, now, 2*time.Second); got.Action != SameKeyResubmit || got.Basis != wantBasis || got.CallTimeout != time.Second {
			t.Fatalf("valid %s proof rejected or call extends beyond proof: %+v", kind, got)
		}
		if got := Decide(state, now.Add(time.Second), time.Second); got.Action != Query {
			t.Fatalf("expiry boundary authorized resubmit: %+v", got)
		}
		for _, field := range []string{"operation", "attempt", "connection", "revision", "namespace", "provider", "key", "payload"} {
			t.Run(string(kind)+"/"+field, func(t *testing.T) {
				evidence := *state.Absence
				switch field {
				case "operation":
					evidence.Identity.OperationID += "_other"
				case "attempt":
					evidence.Identity.AttemptID += "_other"
				case "connection":
					evidence.Identity.ConnectionID += "_other"
				case "revision":
					evidence.Identity.ConnectionRevision += "_other"
				case "namespace":
					evidence.Identity.NamespaceID += "_other"
				case "provider":
					evidence.Identity.Provider += "_other"
				case "key":
					evidence.Identity.Key += "_other"
				case "payload":
					evidence.Identity.PayloadHash += "_other"
				}
				changed := state
				changed.Absence = &evidence
				if got := Decide(changed, now, time.Second); got.Action != Stop || !got.RequiresReview {
					t.Fatalf("identity drift authorized recovery: %+v", got)
				}
			})
		}
	}
}

func TestDurableNeverDispatchedClaimHasFirstGatewayIOBasis(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	state := validState(now)
	state.Status = "created"
	state.Certainty = "none"
	state.FirstDispatchedAt = nil
	plan := Decide(state, now, time.Second)
	if plan.Action != SameKeyResubmit || plan.Basis != ClaimFirstGatewayIO {
		t.Fatalf("durable no-dispatch basis=%+v", plan)
	}
}

func TestUnprovenOrMalformedAbsenceCannotBecomeResubmit(t *testing.T) {
	now := time.Now().UTC()
	for _, evidence := range []AbsenceEvidence{
		{Kind: "http_404", ObservedAt: now, ValidUntil: now.Add(time.Second)},
		{Kind: ProviderConclusiveAbsent, ObservedAt: now.Add(time.Second), ValidUntil: now.Add(2 * time.Second)},
		{Kind: ProviderConclusiveAbsent, ObservedAt: now, ValidUntil: now},
	} {
		state := validState(now)
		evidence.Identity = state.Identity
		state.Absence = &evidence
		if got := Decide(state, now, time.Second); got.Action != Stop || !got.RequiresReview {
			t.Fatalf("malformed evidence was accepted: %+v", got)
		}
	}
}

func TestCallBudgetAndJitterCannotExtendDeadline(t *testing.T) {
	now := time.Now().UTC()
	state := validState(now)
	state.DeadlineAt = now.Add(5 * time.Millisecond)
	if got := Decide(state, now, time.Second); got.CallTimeout != 5*time.Millisecond {
		t.Fatalf("call exceeds operation deadline: %+v", got)
	}
	for _, ordinal := range []int{-1, 0, 1, 2, 8, 1000} {
		first := NextAttemptAt("op_1", ordinal, now, now.Add(time.Hour))
		if first != NextAttemptAt("op_1", ordinal, now, now.Add(time.Hour)) || !first.After(now) || first.After(now.Add(256*time.Second)) {
			t.Fatalf("non-deterministic or unbounded jitter ordinal=%d next=%s", ordinal, first)
		}
		if clipped := NextAttemptAt("op_1", ordinal, now, state.DeadlineAt); !clipped.Equal(state.DeadlineAt) {
			t.Fatalf("backoff exceeded deadline: %s", clipped)
		}
	}
}
