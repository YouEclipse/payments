package clients

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func v2Context(t *testing.T) routingcontext.Context {
	t.Helper()
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	builder, err := routingcontext.NewBuilder([]routingcontext.MerchantProfile{{MerchantID: "merchant_demo", EntityID: "merchant_demo_us", Country: "US", Version: "merchant_config.v1", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}, routingcontext.SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := builder.Build(t.Context(), routingcontext.Input{PaymentID: "pi_test", AttemptID: "att_test", MerchantID: "merchant_demo", AmountMinor: 5200, Currency: "USD", CaptureMethod: "automatic", PaymentMethodToken: "pm_sim_approved"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func v2Decision(t *testing.T, snapshot routingcontext.Context) RoutingDecision {
	t.Helper()
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	decision := RoutingDecision{SelectedCandidate: RouteCandidate{CandidateID: "conn_us", ConnectionRevision: "v1", ProviderNamespaceID: "account_us", Provider: "simulator_a", MerchantAccount: "merchant_us", Connector: "simulator", Region: "us"}, CandidateOrder: []string{"conn_us"}, Exclusions: []CandidateExclusion{}, PolicyVersion: "rules.v1", ContextSchemaVersion: routingcontext.SchemaVersion, ContextDigest: digest, ExecutableUntil: snapshot.BuiltAt.Add(time.Minute), RuleSetVersion: "rules.v1", RuleSetDigest: "sha256:" + strings.Repeat("1", 64), CapabilityCatalogVersion: "catalog.v1", CapabilityCatalogDigest: "sha256:" + strings.Repeat("2", 64), RuleHits: []RuleHit{}, Eligibility: []EligibilityEvidence{{CandidateID: "conn_us", Status: "eligible"}}, ReasonCodes: []string{}}
	decision.DecisionID = versionedDecisionContentID(decision)
	return decision
}

func freezeRouterClientAt(client *RouterClient, at time.Time) {
	client.now = func() time.Time { return at }
}

func TestVersionedDecisionContentIDSurvivesPaymentSnapshotRoundTrip(t *testing.T) {
	decision := v2Decision(t, v2Context(t))
	payload, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	var restored RoutingDecision
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	if !validVersionedDecisionID(restored) {
		t.Fatalf("stored decision no longer matches %s: %s", decision.DecisionID, payload)
	}
}

func TestRouterV2SendsOnlyTheFrozenNonCredentialEnvelope(t *testing.T) {
	snapshot := v2Context(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v2/routing-decisions" {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(payload), "pm_sim_") || strings.Contains(string(payload), "payment_method_token") {
			t.Error("credential crossed Router boundary")
		}
		var received routingcontext.Context
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&received); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(v2Decision(t, received))
	}))
	defer server.Close()
	client, err := NewRouterClient(server.URL, "internal-test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	freezeRouterClientAt(client, snapshot.BuiltAt)
	decision, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot})
	if err != nil || decision.SelectedCandidate.CandidateID != "conn_us" {
		t.Fatalf("decision=%+v, err=%v", decision, err)
	}
}

func TestRouterV2RejectsMismatchedEvidenceAndNeverFallsBackToV1(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		mutate func(*RoutingDecision)
	}{
		{"digest drift", 200, func(d *RoutingDecision) { d.ContextDigest = "sha256:wrong" }},
		{"missing catalog", 200, func(d *RoutingDecision) { d.CapabilityCatalogVersion = "" }},
		{"policy drift", 200, func(d *RoutingDecision) { d.PolicyVersion = "rules.v2" }},
		{"malformed digest", 200, func(d *RoutingDecision) { d.RuleSetDigest = "sha256:rule" }},
		{"rejected decision with winner", 422, func(*RoutingDecision) {}},
		{"duplicate order", 200, func(d *RoutingDecision) { d.CandidateOrder = []string{"conn_us", "conn_us"} }},
		{"missing eligibility", 200, func(d *RoutingDecision) { d.Eligibility = nil }},
		{"ineligible winner", 200, func(d *RoutingDecision) { d.Eligibility[0].Status = "ineligible" }},
		{"unknown winner", 200, func(d *RoutingDecision) { d.Eligibility[0].Status = "unknown" }},
		{"duplicate evidence", 200, func(d *RoutingDecision) { d.Eligibility = append(d.Eligibility, d.Eligibility[0]) }},
		{"v2 unavailable", 503, func(*RoutingDecision) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := v2Context(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/internal/v2/routing-decisions" {
					t.Error("silent legacy fallback")
				}
				d := v2Decision(t, snapshot)
				test.mutate(&d)
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(d)
			}))
			defer server.Close()
			client, err := NewRouterClient(server.URL, "internal-test", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			freezeRouterClientAt(client, snapshot.BuiltAt)
			if _, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot}); !errors.Is(err, ErrUpstream) || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestRouterV2NoEligiblePreservesEvidence(t *testing.T) {
	snapshot := v2Context(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		decision := v2Decision(t, snapshot)
		decision.SelectedCandidate = RouteCandidate{}
		decision.CandidateOrder = []string{}
		decision.ReasonCodes = []string{"no_eligible_candidate"}
		decision.Error = &RouterErrorEvidence{Code: "no_eligible_provider", Message: "no eligible provider"}
		decision.DecisionID = versionedDecisionContentID(decision)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(decision)
	}))
	defer server.Close()
	client, err := NewRouterClient(server.URL, "internal-test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	freezeRouterClientAt(client, snapshot.BuiltAt)
	decision, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot})
	if !errors.Is(err, ErrNoEligibleProvider) || decision.ContextDigest == "" || decision.RuleSetVersion != "rules.v1" {
		t.Fatalf("no-eligible lost evidence: %+v, %v", decision, err)
	}
}

func TestRouterV2RejectsExpiredOrExtendedExecutionAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		deadline func(routingcontext.Context) time.Time
	}{
		{"missing", func(routingcontext.Context) time.Time { return time.Time{} }},
		{"expired at receipt", func(snapshot routingcontext.Context) time.Time { return snapshot.BuiltAt }},
		{"sub-microsecond persistence ambiguity", func(snapshot routingcontext.Context) time.Time {
			return snapshot.BuiltAt.Add(time.Minute + time.Nanosecond)
		}},
		{"extends context", func(snapshot routingcontext.Context) time.Time {
			return snapshot.BuiltAt.Add(routingcontext.OnlineMaxAge + time.Nanosecond)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := v2Context(t)
			decision := v2Decision(t, snapshot)
			decision.ExecutableUntil = test.deadline(snapshot)
			decision.DecisionID = versionedDecisionContentID(decision)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(decision) }))
			defer server.Close()
			client, _ := NewRouterClient(server.URL, "internal-test", server.Client())
			freezeRouterClientAt(client, snapshot.BuiltAt)
			if _, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot}); !errors.Is(err, ErrUpstream) {
				t.Fatalf("invalid execution authority accepted: %v", err)
			}
		})
	}
}
