package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func readV3ContractGolden(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile("../../../../contracts/internal/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func v3SuccessContextGolden(t *testing.T) routingcontext.Context {
	t.Helper()
	var snapshot routingcontext.Context
	decoder := json.NewDecoder(bytes.NewReader(readV3ContractGolden(t, "routing-context-v2.json")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatal("routing-context-v2 golden must contain exactly one object")
	}
	if _, err := snapshot.Digest(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func v3SuccessDecisionGolden(t *testing.T) RoutingDecision {
	t.Helper()
	var decision RoutingDecision
	decoder := json.NewDecoder(bytes.NewReader(readV3ContractGolden(t, "routing-decision-v3-success.json")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatal("routing-decision-v3 success golden must contain exactly one object")
	}
	if !validVersionedDecisionID(decision) {
		t.Fatalf("shared success golden has an invalid content-bound id %q", decision.DecisionID)
	}
	return decision
}

func v3Context(t *testing.T, amount int64) routingcontext.Context {
	t.Helper()
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	builder, err := routingcontext.NewBuilder([]routingcontext.MerchantProfile{{MerchantID: "merchant_demo", EntityID: "merchant_demo_us", Country: "US", Version: "merchant_config.v1", ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}, routingcontext.SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	builder, err = builder.WithRisk(routingcontext.DevelopmentRisk{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := builder.Build(t.Context(), routingcontext.Input{PaymentID: "pi_test", AttemptID: "att_test", MerchantID: "merchant_demo", AmountMinor: amount, Currency: "USD", CaptureMethod: "automatic", PaymentMethodToken: "pm_sim_approved"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func v3RiskRejection(t *testing.T, snapshot routingcontext.Context) RoutingDecision {
	t.Helper()
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	subject, err := snapshot.RiskSubjectDigest()
	if err != nil {
		t.Fatal(err)
	}
	risk, err := riskdecision.EvaluateSale(snapshot.BuiltAt, subject, *snapshot.Risk)
	if err != nil {
		t.Fatal(err)
	}
	if risk.Continue {
		t.Fatal("risk rejection fixture requires a non-allow action")
	}
	result := RoutingDecision{
		Error:          &RouterErrorEvidence{Code: "no_eligible_provider", Message: "risk guardrail stopped routing"},
		CandidateOrder: []string{}, Exclusions: []CandidateExclusion{}, PolicyVersion: "rules.v1",
		ContextSchemaVersion: routingcontext.RiskSchemaVersion, ContextDigest: digest, ExecutableUntil: snapshot.BuiltAt.Add(time.Minute),
		RuleSetVersion: "rules.v1", RuleSetDigest: artifactDigest("1"), CapabilityCatalogVersion: "catalog.v1", CapabilityCatalogDigest: artifactDigest("2"),
		RuleHits: []RuleHit{}, Eligibility: []EligibilityEvidence{{CandidateID: "conn_us", Status: "eligible", Reasons: []ExclusionReason{}}},
		Risk: &risk, ReasonCodes: []string{"risk_" + risk.Outcome, "no_eligible_provider"},
	}
	result.DecisionID = versionedDecisionContentID(result)
	return result
}

func artifactDigest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

func mutateOptimizationEvidence(t *testing.T, decision *RoutingDecision, mutate func(map[string]any)) {
	t.Helper()
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(decision.Optimization))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	mutate(payload)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	decision.Optimization = encoded
}

func firstOptimizationRow(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	scoring, ok := payload["scoring"].(map[string]any)
	if !ok {
		t.Fatal("optimization fixture omitted scoring")
	}
	rows, ok := scoring["breakdown"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatal("optimization fixture omitted score rows")
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatal("optimization fixture score row is not an object")
	}
	return row
}

func TestRouterV3ConsumesSharedRouterSuccessGoldenWithoutLoss(t *testing.T) {
	snapshot := v3SuccessContextGolden(t)
	contextGolden := bytes.TrimSpace(readV3ContractGolden(t, "routing-context-v2.json"))
	decisionGolden := readV3ContractGolden(t, "routing-decision-v3-success.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v3/routing-decisions" {
			t.Errorf("wrong endpoint %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, contextGolden) || strings.Contains(string(body), "pm_sim_") || strings.Contains(string(body), "payment_method_token") {
			t.Errorf("Payment did not send the exact credential-free context golden: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(decisionGolden)
	}))
	defer server.Close()
	client, err := NewRouterClient(server.URL, "internal-test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	freezeRouterClientAt(client, snapshot.BuiltAt)
	decision, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot})
	if err != nil || decision.SelectedCandidate.CandidateID != "conn_us" || decision.Risk == nil || !decision.Risk.Continue || len(decision.Optimization) == 0 || !decision.ExecutableUntil.Equal(snapshot.BuiltAt.Add(5*time.Minute)) || !validVersionedDecisionID(decision) {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	roundTrip, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(roundTrip, bytes.TrimSpace(decisionGolden)) {
		t.Fatalf("Payment changed Router's canonical evidence while decoding it\nwant: %s\n got: %s", decisionGolden, roundTrip)
	}
}

func TestVersionedDecisionContentIDUsesOptimizationSemanticsAcrossObjectReordering(t *testing.T) {
	decision := v3SuccessDecisionGolden(t)
	originalOptimization := append([]byte(nil), decision.Optimization...)
	mutateOptimizationEvidence(t, &decision, func(map[string]any) {})
	if bytes.Equal(decision.Optimization, originalOptimization) {
		t.Fatal("test fixture did not reorder optimization object members")
	}
	if !validVersionedDecisionID(decision) {
		t.Fatal("semantic optimization evidence lost its Router decision ID after JSONB-style object reordering")
	}

	mutateOptimizationEvidence(t, &decision, func(payload map[string]any) {
		firstOptimizationRow(t, payload)["numbers"].(map[string]any)["total_points"] = json.Number("6757")
	})
	if validVersionedDecisionID(decision) {
		t.Fatal("field mutation retained the original Router decision ID")
	}
}

func TestVersionedDecisionContentIDRejectsAmbiguousOptimizationJSON(t *testing.T) {
	base := v3SuccessDecisionGolden(t)
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{"duplicate member", bytes.Replace(base.Optimization, []byte(`{"config_version":`), []byte(`{"config_version":"shadow","config_version":`), 1)},
		{"escaped duplicate member", bytes.Replace(base.Optimization, []byte(`{"config_version":`), []byte(`{"config_version":"shadow","config_\u0076ersion":`), 1)},
		{"non-canonical case alias", bytes.Replace(base.Optimization, []byte(`"config_version"`), []byte(`"Config_version"`), 1)},
		{"multiple values", append(append([]byte(nil), base.Optimization...), []byte(` {}`)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := base
			decision.Optimization = test.payload
			if id := versionedDecisionContentID(decision); id != "" || validVersionedDecisionID(decision) {
				t.Fatalf("ambiguous optimization produced a decision ID %q", id)
			}
		})
	}
}

func TestRouterV3RiskActionsCannotCreateAnExecutableCandidate(t *testing.T) {
	for _, test := range []struct {
		amount int64
		want   error
	}{{49001, ErrRiskBlocked}, {49002, ErrAuthenticationRequired}, {49003, ErrRiskReviewRequired}} {
		t.Run(test.want.Error(), func(t *testing.T) {
			snapshot := v3Context(t, test.amount)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(v3RiskRejection(t, snapshot))
			}))
			defer server.Close()
			client, _ := NewRouterClient(server.URL, "internal-test", server.Client())
			freezeRouterClientAt(client, snapshot.BuiltAt)
			decision, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot})
			if !errors.Is(err, test.want) || decision.SelectedCandidate != (RouteCandidate{}) || len(decision.CandidateOrder) != 0 || len(decision.Optimization) != 0 {
				t.Fatalf("risk action was executable: %+v, %v", decision, err)
			}
		})
	}
}

func TestRouterV3RejectsTamperedOrCredentialLikeEvidence(t *testing.T) {
	snapshot := v3SuccessContextGolden(t)
	base := v3SuccessDecisionGolden(t)
	for _, test := range []struct {
		name      string
		mutate    func(*RoutingDecision)
		recompute bool
	}{
		{"raw changed without content id", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["numbers"].(map[string]any)["total_points"] = json.Number("6757")
			})
		}, false},
		{"deadline changed without content id", func(d *RoutingDecision) {
			d.ExecutableUntil = d.ExecutableUntil.Add(-time.Second)
		}, false},
		{"risk changed with a recomputed content id", func(d *RoutingDecision) {
			risk := *d.Risk
			risk.DecisionDigest = artifactDigest("a")
			d.Risk = &risk
		}, true},
		{"optimization removed with a recomputed content id", func(d *RoutingDecision) {
			d.Optimization = nil
		}, true},
		{"credential-like scalar", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["cost"].(map[string]any)["artifact_version"] = "pm_sim_secret"
			})
		}, true},
		{"identity drift", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["connection_revision"] = "other_v1"
			})
		}, true},
		{"unknown debug field", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) { payload["debug_token"] = "secret" })
		}, true},
		{"score out of range", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["numbers"].(map[string]any)["total_points"] = json.Number("10001")
			})
		}, true},
		{"unbound success error", func(d *RoutingDecision) { d.Error = &RouterErrorEvidence{Code: "debug", Message: "pm_sim_secret"} }, false},
		{"separator PAN", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["cost"].(map[string]any)["artifact_version"] = "4242-4242-4242-4242"
			})
		}, true},
		{"prefixed separator PAN", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["cost"].(map[string]any)["artifact_version"] = "debug:4242-4242-4242-4242"
			})
		}, true},
		{"embedded credential prefix", func(d *RoutingDecision) {
			mutateOptimizationEvidence(t, d, func(payload map[string]any) {
				firstOptimizationRow(t, payload)["cost"].(map[string]any)["artifact_version"] = "debug_pm_sim_secret"
			})
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := base
			test.mutate(&decision)
			if test.recompute {
				decision.DecisionID = versionedDecisionContentID(decision)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(decision) }))
			defer server.Close()
			client, _ := NewRouterClient(server.URL, "internal-test", server.Client())
			freezeRouterClientAt(client, snapshot.BuiltAt)
			if _, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot}); !errors.Is(err, ErrUpstream) {
				t.Fatalf("tampered evidence accepted: %v", err)
			}
		})
	}
}

func TestRouterV3RejectsIncompleteOrStateContradictoryOptimizationEvidence(t *testing.T) {
	snapshot := v3SuccessContextGolden(t)
	base := v3SuccessDecisionGolden(t)
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"empty known cost estimate", func(payload map[string]any) {
			row := firstOptimizationRow(t, payload)
			row["cost"].(map[string]any)["estimate"] = map[string]any{}
		}},
		{"numbers contain only total points", func(payload map[string]any) {
			firstOptimizationRow(t, payload)["numbers"] = map[string]any{"total_points": json.Number("8860")}
		}},
		{"score omits required reason codes", func(payload map[string]any) {
			delete(payload["scoring"].(map[string]any), "reason_codes")
		}},
		{"score reason code has wrong scalar type", func(payload map[string]any) {
			payload["scoring"].(map[string]any)["reason_codes"] = []any{json.Number("1")}
		}},
		{"score timestamp has wrong scalar type", func(payload map[string]any) {
			payload["scoring"].(map[string]any)["evaluated_at"] = true
		}},
		{"score number has wrong scalar type", func(payload map[string]any) {
			firstOptimizationRow(t, payload)["numbers"].(map[string]any)["total_points"] = "8860"
		}},
		{"cost micros has wrong scalar type", func(payload map[string]any) {
			cost := firstOptimizationRow(t, payload)["cost"].(map[string]any)
			cost["estimate"].(map[string]any)["attempt_cost_micros"] = "100000"
		}},
		{"known cost omits estimate", func(payload map[string]any) {
			delete(firstOptimizationRow(t, payload)["cost"].(map[string]any), "estimate")
		}},
		{"unknown cost retains estimate", func(payload map[string]any) {
			firstOptimizationRow(t, payload)["cost"].(map[string]any)["state"] = "unknown"
		}},
		{"known performance omits source segment", func(payload map[string]any) {
			delete(firstOptimizationRow(t, payload)["performance"].(map[string]any), "source_segment")
		}},
		{"prior performance retains bucket segment", func(payload map[string]any) {
			performance := firstOptimizationRow(t, payload)["performance"].(map[string]any)
			performance["state"], performance["source"], performance["fallback_level"] = "prior", "global_prior", json.Number("4")
		}},
		{"not applicable classification retains value", func(payload map[string]any) {
			payload["classifications"].([]any)[0].(map[string]any)["value"] = false
		}},
		{"fixed cost item retains percentage metadata", func(payload map[string]any) {
			cost := firstOptimizationRow(t, payload)["cost"].(map[string]any)
			estimate := cost["estimate"].(map[string]any)
			estimate["breakdown"] = []any{map[string]any{
				"scenario": "attempt", "kind": "fixed", "basis": "contract_fixed", "source_currency": "USD",
				"source_micros": json.Number("100000"), "quote_currency": "USD", "amount_micros": json.Number("100000"), "percentage_round": "half_up",
			}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := base
			mutateOptimizationEvidence(t, &decision, test.mutate)
			decision.DecisionID = versionedDecisionContentID(decision)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(decision) }))
			defer server.Close()
			client, _ := NewRouterClient(server.URL, "internal-test", server.Client())
			freezeRouterClientAt(client, snapshot.BuiltAt)
			if _, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot}); !errors.Is(err, ErrUpstream) {
				t.Fatalf("incomplete or contradictory evidence accepted: %v", err)
			}
		})
	}
}

func TestRouterV3AcceptsCompletePriorAsWholeBatchFallback(t *testing.T) {
	snapshot := v3SuccessContextGolden(t)
	decision := v3SuccessDecisionGolden(t)
	mutateOptimizationEvidence(t, &decision, func(payload map[string]any) {
		score := payload["scoring"].(map[string]any)
		score["status"], score["mode"] = "static_fallback", "static_order"
		row := firstOptimizationRow(t, payload)
		delete(row, "numbers")
		performance := row["performance"].(map[string]any)
		performance["state"], performance["source"], performance["fallback_level"] = "prior", "global_prior", json.Number("4")
		delete(performance, "source_segment")
		delete(performance, "parent_segment")
	})
	decision.DecisionID = versionedDecisionContentID(decision)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(decision) }))
	defer server.Close()
	client, _ := NewRouterClient(server.URL, "internal-test", server.Client())
	freezeRouterClientAt(client, snapshot.BuiltAt)
	if _, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot}); err != nil {
		t.Fatalf("complete prior fallback was rejected: %v", err)
	}
}

func TestRouterV3DoesNotInterpretTypedCountersAsCredentials(t *testing.T) {
	snapshot := v3SuccessContextGolden(t)
	decision := v3SuccessDecisionGolden(t)
	mutateOptimizationEvidence(t, &decision, func(payload map[string]any) {
		performance := firstOptimizationRow(t, payload)["performance"].(map[string]any)
		performance["estimate"].(map[string]any)["sample_size"] = json.Number("4000000000006")
	})
	decision.DecisionID = versionedDecisionContentID(decision)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(decision) }))
	defer server.Close()
	client, _ := NewRouterClient(server.URL, "internal-test", server.Client())
	freezeRouterClientAt(client, snapshot.BuiltAt)
	if _, err := client.Decide(t.Context(), RoutingRequest{Context: &snapshot}); err != nil {
		t.Fatalf("legitimate typed counter rejected because it happens to pass Luhn: %v", err)
	}
}
