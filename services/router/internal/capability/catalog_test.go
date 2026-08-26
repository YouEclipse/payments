package capability_test

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/capability"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func TestCatalogEligibilityUsesExactConnectionAndThreeValues(t *testing.T) {
	for _, test := range []struct {
		name           string
		change         func(*capability.Artifact, *routingcontext.Context)
		status, reason string
	}{
		{"supported", func(*capability.Artifact, *routingcontext.Context) {}, capability.Eligible, ""},
		{"disabled", func(a *capability.Artifact, _ *routingcontext.Context) { a.Descriptors[0].Enabled = "no" }, capability.Ineligible, "provider_disabled"},
		{"unknown enable", func(a *capability.Artifact, _ *routingcontext.Context) { a.Descriptors[0].Enabled = "unknown" }, capability.Unknown, "capability_unknown"},
		{"currency", func(_ *capability.Artifact, c *routingcontext.Context) { c.Base.Currency = "GBP" }, capability.Ineligible, "currency_not_supported"},
		{"currency empty", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].Currencies = testfixture.Known()
		}, capability.Ineligible, "currency_not_supported"},
		{"currency unknown", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].Currencies = capability.Set{State: "unknown"}
		}, capability.Unknown, "capability_unknown"},
		{"country", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].Countries = testfixture.Known("GB")
		}, capability.Ineligible, "country_not_supported"},
		{"issuer has no fallback", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].CountryField = "payment_method_context.issuer_country"
		}, capability.Unknown, "capability_unknown"},
		{"method", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].PaymentMethods = testfixture.Known()
		}, capability.Ineligible, "payment_method_not_supported"},
		{"operation", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].Operations = testfixture.Known("authorize")
		}, capability.Ineligible, "operation_not_supported"},
		{"capture", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].CaptureMethods = testfixture.Known("manual")
		}, capability.Ineligible, "capture_method_not_supported"},
		{"credential mismatch", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].CredentialDomains = testfixture.Known("another_account")
		}, capability.Ineligible, "token_not_portable"},
		{"credential unknown", func(_ *capability.Artifact, c *routingcontext.Context) {
			c.PaymentMethod.CredentialDomain = routingcontext.Unknown("vault", "v1", c.BuiltAt)
		}, capability.Unknown, "token_binding_unknown"},
		{"credential stale", func(_ *capability.Artifact, c *routingcontext.Context) {
			c.PaymentMethod.CredentialDomain.State = "stale"
			c.PaymentMethod.CredentialDomain.ObservedAt = c.BuiltAt.Add(-time.Hour)
			expiry := c.BuiltAt
			c.PaymentMethod.CredentialDomain.ExpiresAt = &expiry
		}, capability.Unknown, "token_binding_unknown"},
		{"future effective", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].EffectiveFrom = testfixture.Now.Add(time.Minute)
		}, capability.Unknown, "capability_snapshot_stale"},
		{"expired descriptor", func(a *capability.Artifact, _ *routingcontext.Context) { a.Descriptors[0].ExpiresAt = testfixture.Now }, capability.Unknown, "capability_snapshot_stale"},
		{"expired catalog", func(a *capability.Artifact, _ *routingcontext.Context) { a.ExpiresAt = testfixture.Now }, capability.Unknown, "capability_snapshot_stale"},
		{"known no wins unknown", func(a *capability.Artifact, _ *routingcontext.Context) {
			a.Descriptors[0].Enabled = "no"
			a.Descriptors[0].Currencies = capability.Set{State: "unknown"}
		}, capability.Ineligible, "provider_disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact, context := testfixture.Catalog(), testfixture.Context()
			test.change(&artifact, &context)
			if err := context.Validate(); err != nil {
				t.Fatal(err)
			}
			catalog, err := capability.Compile(artifact)
			if err != nil {
				t.Fatal(err)
			}
			var actual capability.Result
			for _, result := range catalog.Filter(context) {
				if result.CandidateID == "conn_us" {
					actual = result
				}
			}
			if actual.Status != test.status {
				t.Fatalf("status=%s, want %s: %+v", actual.Status, test.status, actual)
			}
			if test.reason != "" {
				found := false
				for _, reason := range actual.Reasons {
					if reason.Code == test.reason {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing reason %s: %+v", test.reason, actual)
				}
			}
		})
	}
	for _, currency := range []string{"USD", "EUR"} {
		context := testfixture.Context()
		context.Base.Currency = currency
		catalog, err := capability.Compile(testfixture.Catalog())
		if err != nil {
			t.Fatal(err)
		}
		eligible := []string{}
		for _, result := range catalog.Filter(context) {
			if result.Status == capability.Eligible {
				eligible = append(eligible, result.CandidateID)
			}
		}
		wanted := "conn_us"
		if currency == "EUR" {
			wanted = "conn_eu"
		}
		if !reflect.DeepEqual(eligible, []string{wanted}) {
			t.Fatalf("%s eligible=%v", currency, eligible)
		}
	}
}

func TestCatalogRejectsPartialAndAmbiguousArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*capability.Artifact)
	}{
		{"schema", func(a *capability.Artifact) { a.SchemaVersion = "capability_catalog.v99" }},
		{"missing version", func(a *capability.Artifact) { a.Version = "" }},
		{"partial descriptor", func(a *capability.Artifact) { a.Descriptors = a.Descriptors[:1] }},
		{"duplicate descriptor", func(a *capability.Artifact) { a.Descriptors[1].CandidateID = a.Descriptors[0].CandidateID }},
		{"unknown descriptor", func(a *capability.Artifact) { a.Descriptors[0].CandidateID = "absent" }},
		{"duplicate candidate", func(a *capability.Artifact) { a.Candidates[1].ID = a.Candidates[0].ID }},
		{"alias collision", func(a *capability.Artifact) {
			alias := a.Candidates[0]
			alias.ID = a.Candidates[1].ID
			a.Candidates[1] = alias
		}},
		{"revision absent", func(a *capability.Artifact) { a.Candidates[0].ConnectionRevision = "" }},
		{"noncanonical identity", func(a *capability.Artifact) { a.Candidates[0].ID = "Conn_US" }},
		{"namespace collision", func(a *capability.Artifact) { a.Candidates[1].Provider = "different_psp" }},
		{"missing enable", func(a *capability.Artifact) { a.Descriptors[0].Enabled = "" }},
		{"missing state", func(a *capability.Artifact) { a.Descriptors[0].Currencies.State = "" }},
		{"unknown with values", func(a *capability.Artifact) { a.Descriptors[0].Currencies.State = "unknown" }},
		{"wildcard", func(a *capability.Artifact) { a.Descriptors[0].Currencies = testfixture.Known("*") }},
		{"duplicate currency", func(a *capability.Artifact) { a.Descriptors[0].Currencies = testfixture.Known("USD", "USD") }},
		{"ambiguous country", func(a *capability.Artifact) { a.Descriptors[0].CountryField = "country" }},
		{"observation after publication", func(a *capability.Artifact) { a.Descriptors[0].ObservedAt = a.PublishedAt.Add(time.Minute) }},
		{"invalid interval", func(a *capability.Artifact) { a.Descriptors[0].ExpiresAt = a.Descriptors[0].EffectiveFrom }},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := testfixture.Catalog()
			test.change(&artifact)
			if _, err := capability.Compile(artifact); err == nil {
				t.Fatal("accepted invalid catalog")
			}
		})
	}
}

func TestCatalogCanonicalDigestAndImmutableConcurrentReads(t *testing.T) {
	artifact := testfixture.Catalog()
	catalog, err := capability.Compile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	canonical := testfixture.Catalog()
	canonical.Candidates[0], canonical.Candidates[1] = canonical.Candidates[1], canonical.Candidates[0]
	canonical.Descriptors[0].Currencies.Values = []string{"BRL", "USD"}
	canonical.Descriptors[0], canonical.Descriptors[1] = canonical.Descriptors[1], canonical.Descriptors[0]
	reordered, err := capability.Compile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Digest() != reordered.Digest() {
		t.Fatal("array/set serialization order changed artifact digest")
	}
	artifact.Descriptors[0].Enabled = "no"
	artifact.Candidates[0].ConnectionRevision = "mutated"
	copy := catalog.Candidates()
	copy[0].ID = "mutated"
	baseline := catalog.Filter(testfixture.Context())
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if !reflect.DeepEqual(baseline, catalog.Filter(testfixture.Context())) {
				t.Error("concurrent evaluation changed immutable catalog")
			}
		}()
	}
	group.Wait()
	if !catalog.CandidatesValidAt([]string{"conn_us"}, testfixture.Now) || catalog.CandidatesValidAt([]string{"unknown"}, testfixture.Now) || catalog.CandidatesValidAt([]string{"conn_us"}, testfixture.Now.Add(time.Hour)) {
		t.Fatal("online descriptor freshness gate failed")
	}
	changed := testfixture.Catalog()
	changed.Candidates[0].ConnectionRevision = "conn_us_v2"
	newCatalog, err := capability.Compile(changed)
	if err != nil {
		t.Fatal(err)
	}
	if newCatalog.Digest() == catalog.Digest() {
		t.Fatal("connection revision was not bound into the catalog digest")
	}
}
