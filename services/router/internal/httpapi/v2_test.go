package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func v2Fixture(t *testing.T, change func(*decision.Config), at time.Time) (*http.ServeMux, *bytes.Buffer) {
	t.Helper()
	cfg := decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
	if change != nil {
		change(&cfg)
	}
	engine, err := decision.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	handler := NewHandler(nil, slog.New(slog.NewTextHandler(logs, nil))).WithV2(engine)
	handler.now = func() time.Time { return at }
	mux := http.NewServeMux()
	handler.Register(mux)
	return mux, logs
}

func v2Post(mux *http.ServeMux, payload string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/internal/v2/routing-decisions", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	return response
}

func TestV2HTTPDecisionAndRejectionCarryCanonicalEvidence(t *testing.T) {
	mux, _ := v2Fixture(t, nil, testfixture.Now)
	for _, currency := range []string{"USD", "EUR", "GBP"} {
		t.Run(currency, func(t *testing.T) {
			payload := strings.Replace(string(testfixture.ContextGolden), `"currency":"USD"`, `"currency":"`+currency+`"`, 1)
			response := v2Post(mux, payload)
			want := http.StatusOK
			if currency == "GBP" {
				want = http.StatusUnprocessableEntity
			}
			if response.Code != want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result decision.Decision
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.PolicyVersion != result.RuleSetVersion || result.ContextSchemaVersion != routingcontext.SchemaVersion || len(result.ContextDigest) != 71 || result.ExecutableUntil.IsZero() || !testfixture.Now.Before(result.ExecutableUntil) || len(result.RuleSetDigest) != 71 || len(result.CapabilityCatalogDigest) != 71 || len(result.Eligibility) != 2 {
				t.Fatal("incomplete v2 response", response.Body.String())
			}
			if currency == "GBP" {
				var envelope noEligibleProviderV2Response
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Error.Code != "no_eligible_provider" || result.SelectedCandidate != nil || len(result.CandidateOrder) != 0 || strings.Contains(response.Body.String(), "selected_candidate") {
					t.Fatal("422 manufactured a selected candidate", response.Body.String())
				}
			} else {
				expected := "conn_us"
				if currency == "EUR" {
					expected = "conn_eu"
				}
				if result.SelectedCandidate == nil || result.SelectedCandidate.ID != expected {
					t.Fatal("wrong connection", response.Body.String())
				}
			}
			for _, field := range []string{`"rule_hits":[]`, `"reason_codes":`, `"exclusions":`, `"eligibility":`} {
				if !strings.Contains(response.Body.String(), field) {
					t.Fatalf("missing canonical array field %s", field)
				}
			}
		})
	}
}

func TestV2StrictHTTPBoundaryRejectsWithoutEchoingUntrustedInput(t *testing.T) {
	mux, logs := v2Fixture(t, nil, testfixture.Now)
	valid := strings.TrimSpace(string(testfixture.ContextGolden))
	for name, payload := range map[string]string{
		"unknown secret field": strings.Replace(valid, `"category":"card"`, `"category":"card","private-token-do-not-log":"secret"`, 1),
		"token property":       strings.Replace(valid, `"category":"card"`, `"category":"card","payment_method_token":"private-token-do-not-log"`, 1),
		"duplicate currency":   strings.Replace(valid, `"currency":"USD"`, `"currency":"EUR","currency":"USD"`, 1),
		"policy injection":     strings.TrimSuffix(valid, "}") + `,"rule_set":{"version":"override"}}`,
		"schema":               strings.Replace(valid, `routing_context.v1`, `routing_context.v2`, 1),
		"known absent value":   strings.Replace(valid, `"value":"US",`, "", 1),
		"unknown has value":    strings.Replace(valid, `"state":"unknown"`, `"state":"unknown","value":"private-token-do-not-log"`, 1),
		"stale known":          strings.Replace(valid, `2026-08-26T01:05:00Z`, `2026-08-26T01:00:00Z`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			response := v2Post(mux, payload)
			if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "private-token") || strings.Contains(logs.String(), "private-token") {
				t.Fatal("unsafe malformed Context response", response.Code, response.Body.String())
			}
		})
	}
	oversized := v2Post(mux, strings.Repeat(" ", maxRequestBodyBytes+1))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("body budget not enforced", oversized.Code)
	}
}

func TestV2UnknownCredentialIsExplicitNoCandidate(t *testing.T) {
	mux, _ := v2Fixture(t, nil, testfixture.Now)
	context := testfixture.Context()
	context.PaymentMethod.CredentialDomain = routingcontext.Unknown("metadata_unavailable", "v1", context.BuiltAt)
	payload, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	response := v2Post(mux, string(payload))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "token_binding_unknown") {
		t.Fatal("unknown credential silently allowed", response.Code, response.Body.String())
	}
}

func TestV2OnlineFreshnessAndMissingConfigNeverFallBackToV1(t *testing.T) {
	server := newTestServer(t)
	response := postJSON(t, server.URL+"/internal/v2/routing-decisions", string(testfixture.ContextGolden))
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(response.Body)
		t.Fatal("v2 missing config used legacy router", response.StatusCode, string(body))
	}
	legacy := postJSON(t, server.URL+"/internal/v1/routing-decisions", validRequestJSON())
	defer legacy.Body.Close()
	if legacy.StatusCode != http.StatusOK {
		t.Fatal("missing v2 config broke explicit v1")
	}
	for _, test := range []struct {
		name   string
		at     time.Time
		change func(*decision.Config)
		status int
	}{
		{"expired Context", testfixture.Now.Add(6 * time.Minute), nil, http.StatusBadRequest},
		{"future Context", testfixture.Now.Add(-time.Nanosecond), nil, http.StatusBadRequest},
		{"expired artifact", testfixture.Now, func(c *decision.Config) { c.RuleSet.ExpiresAt = testfixture.Now }, http.StatusServiceUnavailable},
		{"expired candidate since build", testfixture.Now.Add(time.Minute), func(c *decision.Config) {
			c.CapabilityCatalog.Descriptors[0].ExpiresAt = testfixture.Now.Add(30 * time.Second)
		}, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			mux, _ := v2Fixture(t, test.change, test.at)
			result := v2Post(mux, string(testfixture.ContextGolden))
			if result.Code != test.status {
				t.Fatal(result.Code, result.Body.String())
			}
		})
	}
}

func TestV2MethodAndMediaTypeAreExplicit(t *testing.T) {
	mux, _ := v2Fixture(t, nil, testfixture.Now)
	for _, test := range []struct {
		method, contentType string
		status              int
	}{{http.MethodGet, "application/json", 405}, {http.MethodPost, "text/plain", 415}, {http.MethodPost, "", 415}} {
		request := httptest.NewRequest(test.method, "/internal/v2/routing-decisions", bytes.NewReader(testfixture.ContextGolden))
		request.Header.Set("Content-Type", test.contentType)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestV3RejectsAuthorizeAsAnExplicitUnsupportedContract(t *testing.T) {
	config := decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
	optimization := testfixture.Optimization()
	config.Optimization = &optimization
	engine, err := decision.NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, nil).WithV3(engine)
	handler.now = func() time.Time { return testfixture.Now }
	mux := http.NewServeMux()
	handler.Register(mux)
	context := testfixture.RiskContext("allow")
	context.Base.Operation, context.Base.CaptureMethod = "authorize", "manual"
	payload, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v3/routing-decisions", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"field":"base.operation"`) {
		t.Fatalf("unsupported v3 operation became an internal error: %d %s", response.Code, response.Body.String())
	}
}

func TestV3HTTPSuccessMatchesSharedContractGolden(t *testing.T) {
	config := decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
	optimization := testfixture.Optimization()
	config.Optimization = &optimization
	engine, err := decision.NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, nil).WithV3(engine)
	handler.now = func() time.Time { return testfixture.Now }
	mux := http.NewServeMux()
	handler.Register(mux)

	request := httptest.NewRequest(http.MethodPost, "/internal/v3/routing-decisions", bytes.NewReader(testfixture.RiskContextGolden))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("v3 golden request status=%d body=%s", response.Code, response.Body.String())
	}
	if !bytes.Equal(response.Body.Bytes(), testfixture.RoutingV3SuccessGolden) {
		t.Fatalf("Router v3 response drifted from the shared contract golden\nwant: %s\n got: %s", testfixture.RoutingV3SuccessGolden, response.Body.Bytes())
	}

	var result decision.Decision
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	contentID := result.ID
	result.ID = ""
	canonical, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if want := "rtd_" + hex.EncodeToString(digest[:16]); contentID != want {
		t.Fatalf("decision id is not bound to the complete response: got %s want %s", contentID, want)
	}
	if result.ExecutableUntil != testfixture.Now.Add(5*time.Minute) || result.Risk == nil || result.Risk.Action != "allow" || !result.Risk.Continue || len(result.Risk.RequiredCapabilities) != 0 || result.Optimization == nil {
		t.Fatalf("v3 golden omitted deadline, allow Risk, or Optimization evidence: %+v", result)
	}
	score := result.Optimization.Scoring
	if score.Status != "scored" || score.Mode != "full_v1" || len(score.Breakdown) != 1 || score.Breakdown[0].Numbers == nil {
		t.Fatalf("scored state-dependent shape is incomplete: %+v", score)
	}
	row := score.Breakdown[0]
	if row.Cost.State != "known" || row.Cost.Estimate == nil || len(row.Cost.Estimate.Breakdown) != 6 || row.Performance.State != "known" || row.Performance.Estimate == nil || row.Performance.SourceSegment == nil || row.Performance.ParentSegment != nil {
		t.Fatalf("known feature evidence has the wrong required/optional shape: %+v", row)
	}
	if len(result.Optimization.Classifications) != 1 {
		t.Fatalf("classification evidence count=%d", len(result.Optimization.Classifications))
	}
	classification := result.Optimization.Classifications[0]
	if classification.State != "not_applicable" || classification.Value != nil || classification.Rule != nil || classification.Country != nil {
		t.Fatalf("not_applicable classification retained forbidden optional evidence: %+v", classification)
	}
}

func TestV2RechecksClockWhenEvaluationCrossesExpiryBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*decision.Config)
		end    time.Time
		status int
	}{
		{"descriptor", func(c *decision.Config) {
			c.CapabilityCatalog.Descriptors[0].ExpiresAt = testfixture.Now.Add(time.Nanosecond)
		}, testfixture.Now.Add(2 * time.Nanosecond), 503},
		{"catalog", func(c *decision.Config) { c.CapabilityCatalog.ExpiresAt = testfixture.Now.Add(time.Nanosecond) }, testfixture.Now.Add(2 * time.Nanosecond), 503},
		{"rule set", func(c *decision.Config) { c.RuleSet.ExpiresAt = testfixture.Now.Add(time.Nanosecond) }, testfixture.Now.Add(2 * time.Nanosecond), 503},
		{"context observation", func(*decision.Config) {}, testfixture.Now.Add(5 * time.Minute), 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
			test.change(&config)
			engine, err := decision.NewEngine(config)
			if err != nil {
				t.Fatal(err)
			}
			handler := NewHandler(nil, nil).WithV2(engine)
			calls := 0
			handler.now = func() time.Time {
				calls++
				if calls == 1 {
					return testfixture.Now
				}
				return test.end
			}
			mux := http.NewServeMux()
			handler.Register(mux)
			response := v2Post(mux, string(testfixture.ContextGolden))
			if response.Code != test.status || calls < 2 {
				t.Fatalf("expiry boundary was not checked at completion: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestV2RejectionAlsoRefusesEvidenceThatExpiresDuringEvaluation(t *testing.T) {
	config := decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
	config.RuleSet.ExpiresAt = testfixture.Now.Add(time.Nanosecond)
	engine, err := decision.NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, nil).WithV2(engine)
	calls := 0
	handler.now = func() time.Time {
		calls++
		if calls == 1 {
			return testfixture.Now
		}
		return testfixture.Now.Add(2 * time.Nanosecond)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	payload := strings.Replace(string(testfixture.ContextGolden), `"currency":"USD"`, `"currency":"GBP"`, 1)
	response := v2Post(mux, payload)
	if response.Code != http.StatusServiceUnavailable || calls < 2 {
		t.Fatalf("stale rejection evidence escaped: %d %s", response.Code, response.Body.String())
	}
}

func TestVersionedHandlersRefuseAnEngineForTheWrongContractAndReadinessExpires(t *testing.T) {
	plain, err := decision.NewEngine(decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()})
	if err != nil {
		t.Fatal(err)
	}
	optimizedConfig := decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
	optimization := testfixture.Optimization()
	optimizedConfig.Optimization = &optimization
	optimized, err := decision.NewEngine(optimizedConfig)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, nil).WithV2(optimized).WithV3(plain)
	handler.now = func() time.Time { return testfixture.Now }
	mux := http.NewServeMux()
	handler.Register(mux)
	for _, path := range []string{"/internal/v2/routing-decisions", "/internal/v3/routing-decisions"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s accepted the wrong engine: %d %s", path, response.Code, response.Body.String())
		}
	}
	readyHandler := NewHandler(nil, nil).WithV3(optimized).WithRequiredVersion("v3")
	readyHandler.now = func() time.Time { return testfixture.Now.Add(2 * time.Hour) }
	readyMux := http.NewServeMux()
	readyHandler.Register(readyMux)
	for path, want := range map[string]int{"/livez": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		readyMux.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	freshRules, freshCatalog := testfixture.RuleSet(), testfixture.Catalog()
	freshRules.ExpiresAt, freshCatalog.ExpiresAt = testfixture.Now.Add(3*time.Hour), testfixture.Now.Add(3*time.Hour)
	for index := range freshCatalog.Descriptors {
		freshCatalog.Descriptors[index].ExpiresAt = testfixture.Now.Add(3 * time.Hour)
	}
	freshV2, err := decision.NewEngine(decision.Config{RuleSet: freshRules, CapabilityCatalog: freshCatalog})
	if err != nil {
		t.Fatal(err)
	}
	compatible := NewHandler(nil, nil).WithV2(freshV2).WithV3(optimized).WithRequiredVersion("v2")
	compatible.now = func() time.Time { return testfixture.Now.Add(2 * time.Hour) }
	compatibleMux := http.NewServeMux()
	compatible.Register(compatibleMux)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	compatibleMux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expired optional v3 evicted a v2 deployment: %d %s", response.Code, response.Body.String())
	}
}
