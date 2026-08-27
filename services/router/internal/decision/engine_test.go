package decision_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/capability"
	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/rules"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func config() decision.Config {
	return decision.Config{RuleSet: testfixture.RuleSet(), CapabilityCatalog: testfixture.Catalog()}
}
func newEngine(t *testing.T, cfg decision.Config) *decision.Engine {
	t.Helper()
	engine, err := decision.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestPipelineEvidenceAndCandidateIdentityAreComplete(t *testing.T) {
	engine := newEngine(t, config())
	for _, test := range []struct{ currency, winner string }{{"USD", "conn_us"}, {"EUR", "conn_eu"}, {"GBP", ""}} {
		t.Run(test.currency, func(t *testing.T) {
			context := testfixture.Context()
			context.Base.Currency = test.currency
			result, err := engine.Decide(context)
			if test.winner == "" {
				if !errors.Is(err, routing.ErrNoEligibleProvider) || result.SelectedCandidate != nil || len(result.CandidateOrder) != 0 {
					t.Fatalf("expected no candidate: %+v %v", result, err)
				}
			} else if err != nil || result.SelectedCandidate == nil || result.SelectedCandidate.ID != test.winner {
				t.Fatalf("wrong selected identity %+v %v", result, err)
			}
			if result.PolicyVersion != result.RuleSetVersion || result.ContextSchemaVersion != routingcontext.SchemaVersion || result.ID == "" || result.RuleSetVersion == "" || result.CapabilityCatalogVersion == "" {
				t.Fatal("incomplete version evidence")
			}
			pattern := regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
			for _, digest := range []string{result.ContextDigest, result.RuleSetDigest, result.CapabilityCatalogDigest} {
				if !pattern.MatchString(digest) {
					t.Fatalf("incomplete digest %s", digest)
				}
			}
			if len(result.Eligibility) != 2 {
				t.Fatal("missing eligibility evidence")
			}
			if result.SelectedCandidate != nil {
				selected := result.SelectedCandidate
				if selected.ConnectionRevision != selected.ID+"_v1" || selected.ProviderNamespaceID != "simulator_a" || selected.Provider != "simulator_a" || selected.MerchantAccount == "" || selected.Connector == "" || selected.Region == "" {
					t.Fatal("candidate tuple lost execution identity")
				}
				for _, candidateID := range result.CandidateOrder {
					found := false
					for _, evidence := range result.Eligibility {
						if evidence.CandidateID == candidateID && evidence.Status == capability.Eligible {
							found = true
						}
					}
					if !found {
						t.Fatal("candidate order contains a non-eligible identity")
					}
				}
			}
		})
	}
}

func TestPipelineForceAndSplitNeverOverrideCredentialOrCurrencyEligibility(t *testing.T) {
	for _, action := range []string{"force", "split"} {
		cfg := config()
		rule := testfixture.Rule("must_eu", action, 100, "conn_eu")
		if action == "split" {
			rule.Action = rules.Action{Type: "split", Weights: []rules.Weight{{CandidateID: "conn_eu", Weight: 10000}}}
		}
		cfg.RuleSet.Rules = []rules.Rule{rule}
		engine := newEngine(t, cfg)
		result, err := engine.Decide(testfixture.Context())
		if !errors.Is(err, routing.ErrNoEligibleProvider) || result.SelectedCandidate != nil {
			t.Fatalf("%s revived currency-ineligible candidate", action)
		}
		if len(result.RuleHits) != 1 || result.RuleHits[0].Truth != rules.True {
			t.Fatal("missing matched rule trace")
		}
		context := testfixture.Context()
		context.Base.Currency = "EUR"
		context.PaymentMethod.CredentialDomain = routingcontext.Unknown("vault", "v1", context.BuiltAt)
		result, err = engine.Decide(context)
		if !errors.Is(err, routing.ErrNoEligibleProvider) || result.SelectedCandidate != nil {
			t.Fatalf("%s revived unknown credential domain", action)
		}
	}
}

func TestPipelineCanonicalReplayAndSnapshotCopyAreStable(t *testing.T) {
	cfg := config()
	engine := newEngine(t, cfg)
	context := testfixture.Context()
	baseline, err := engine.Decide(context)
	if err != nil {
		t.Fatal(err)
	}
	reordered := config()
	reordered.CapabilityCatalog.Candidates[0], reordered.CapabilityCatalog.Candidates[1] = reordered.CapabilityCatalog.Candidates[1], reordered.CapabilityCatalog.Candidates[0]
	reordered.CapabilityCatalog.Descriptors[0], reordered.CapabilityCatalog.Descriptors[1] = reordered.CapabilityCatalog.Descriptors[1], reordered.CapabilityCatalog.Descriptors[0]
	second, err := newEngine(t, reordered).Decide(context)
	if err != nil || !reflect.DeepEqual(baseline, second) {
		t.Fatal("serialization order changed decision", err)
	}
	cfg.CapabilityCatalog.Candidates[0].ConnectionRevision = "changed"
	cfg.RuleSet.DefaultCandidateOrder[0] = "changed"
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := engine.Decide(context)
			if err != nil || !reflect.DeepEqual(baseline, result) {
				t.Error("concurrent immutable decision changed", err)
			}
		}()
	}
	group.Wait()
	for _, change := range []func(*decision.Config){func(c *decision.Config) { c.CapabilityCatalog.Candidates[0].ConnectionRevision = "conn_us_v2" }, func(c *decision.Config) {
		c.CapabilityCatalog.Candidates[0].ProviderNamespaceID = "simulator_a_rebound"
	}, func(c *decision.Config) { c.RuleSet.Version = "rules_v2" }} {
		changed := config()
		change(&changed)
		result, err := newEngine(t, changed).Decide(context)
		if err != nil {
			t.Fatal(err)
		}
		if result.ID == baseline.ID {
			t.Fatal("identity or artifact version was absent from decision ID")
		}
	}
}

func TestPipelineReplayCanExplainExpiredSnapshotButOnlineExecutionCannotUseIt(t *testing.T) {
	cfg := config()
	cfg.CapabilityCatalog.Descriptors[0].ExpiresAt = testfixture.Now.Add(30 * time.Second)
	engine := newEngine(t, cfg)
	result, err := engine.Decide(testfixture.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := testfixture.Now.Add(30 * time.Second); !result.ExecutableUntil.Equal(want) {
		t.Fatalf("decision deadline=%s want descriptor deadline %s", result.ExecutableUntil, want)
	}
	if !engine.ExecutableAt(result, testfixture.Now) || engine.ExecutableAt(result, testfixture.Now.Add(time.Minute)) {
		t.Fatal("expired descriptor extended by old Context")
	}
	if engine.ReadyAt(testfixture.Now.Add(time.Hour)) {
		t.Fatal("expired artifact admitted")
	}
	before, _ := json.Marshal(result)
	after, err := engine.Decide(testfixture.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(after)
	if string(before) != string(encoded) {
		t.Fatal("wall clock leaked into replay")
	}
}

func TestExecutableDeadlineCanonicalizesDownToDurableMicroseconds(t *testing.T) {
	cfg := config()
	raw := testfixture.Now.Add(30*time.Second + 123456789*time.Nanosecond)
	cfg.CapabilityCatalog.Descriptors[0].ExpiresAt = raw
	engine := newEngine(t, cfg)
	result, err := engine.Decide(testfixture.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := raw.Truncate(time.Microsecond)
	if !result.ExecutableUntil.Equal(want) || result.ExecutableUntil.After(raw) || result.ExecutableUntil.Nanosecond()%1000 != 0 {
		t.Fatalf("deadline=%s raw=%s want=%s", result.ExecutableUntil, raw, want)
	}
	if !engine.ExecutableAt(result, want.Add(-time.Nanosecond)) || engine.ExecutableAt(result, want) {
		t.Fatal("microsecond canonicalization changed the half-open execution boundary")
	}
}
