// Package capability evaluates immutable, connection-scoped capability facts.
// No provider API, credentials, health or scoring signal belongs in this package.
package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
)

const SchemaVersion = "capability_catalog.v1"
const (
	Eligible   = "eligible"
	Ineligible = "ineligible"
	Unknown    = "unknown"
)

// A known empty set explicitly supports nothing. Missing/unknown is never ANY.
type Set struct {
	State  string   `json:"state"`
	Values []string `json:"values,omitempty"`
}

type Descriptor struct {
	CandidateID       string    `json:"candidate_id"`
	Enabled           string    `json:"enabled"`
	Currencies        Set       `json:"currencies"`
	CountryField      string    `json:"country_field"`
	Countries         Set       `json:"countries"`
	PaymentMethods    Set       `json:"payment_methods"`
	Operations        Set       `json:"operations"`
	CaptureMethods    Set       `json:"capture_methods"`
	CredentialDomains Set       `json:"credential_domains"`
	Source            string    `json:"source"`
	SourceVersion     string    `json:"source_version"`
	ObservedAt        time.Time `json:"observed_at"`
	EffectiveFrom     time.Time `json:"effective_from"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// Candidates is the complete manifest. Exactly one descriptor must exist for
// every identity, so a partially delivered snapshot cannot be installed.
type Artifact struct {
	SchemaVersion string                 `json:"schema_version"`
	Version       string                 `json:"version"`
	PublishedAt   time.Time              `json:"published_at"`
	EffectiveFrom time.Time              `json:"effective_from"`
	ExpiresAt     time.Time              `json:"expires_at"`
	Candidates    []routing.CandidateRef `json:"candidates"`
	Descriptors   []Descriptor           `json:"descriptors"`
}

type Result struct {
	CandidateID string           `json:"candidate_id"`
	Status      string           `json:"status"`
	Reasons     []routing.Reason `json:"reasons"`
}

type Catalog struct {
	artifact Artifact
	digest   string
}

var platformID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func Compile(input Artifact) (*Catalog, error) {
	// Take ownership of every slice before canonical sorting.
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, errors.New("invalid capability artifact")
	}
	var artifact Artifact
	if err := json.Unmarshal(encoded, &artifact); err != nil {
		return nil, errors.New("invalid capability artifact")
	}
	if artifact.SchemaVersion != SchemaVersion || !routingcontext.ValidIdentifier(artifact.Version) ||
		!interval(artifact.PublishedAt, artifact.EffectiveFrom, artifact.ExpiresAt) ||
		len(artifact.Candidates) == 0 || len(artifact.Candidates) > 128 || len(artifact.Descriptors) != len(artifact.Candidates) {
		return nil, errors.New("capability catalog requires a version, valid interval and complete bounded candidate manifest")
	}
	identities := make(map[string]bool)
	execution := make(map[[4]string]bool)
	namespaces := make(map[string]string)
	for _, candidate := range artifact.Candidates {
		if !platformID.MatchString(candidate.ID) || !platformID.MatchString(candidate.ConnectionRevision) || !platformID.MatchString(candidate.ProviderNamespaceID) || identities[candidate.ID] {
			return nil, errors.New("invalid or duplicate capability candidate identity")
		}
		for _, value := range []string{candidate.Provider, candidate.MerchantAccount, candidate.Connector, candidate.Region} {
			if !routingcontext.ValidIdentifier(value) || strings.TrimSpace(value) != value {
				return nil, errors.New("incomplete capability execution identity")
			}
		}
		tuple := [4]string{candidate.Provider, candidate.MerchantAccount, candidate.Connector, candidate.Region}
		if execution[tuple] {
			return nil, errors.New("capability manifest contains aliases of one execution identity")
		}
		if provider, exists := namespaces[candidate.ProviderNamespaceID]; exists && provider != candidate.Provider {
			return nil, errors.New("capability namespace contains different provider kinds")
		}
		namespaces[candidate.ProviderNamespaceID], identities[candidate.ID], execution[tuple] = candidate.Provider, true, true
	}
	seen := make(map[string]bool)
	for index := range artifact.Descriptors {
		d := &artifact.Descriptors[index]
		if !identities[d.CandidateID] || seen[d.CandidateID] {
			return nil, errors.New("capability descriptor identity is absent or duplicated")
		}
		seen[d.CandidateID] = true
		if !oneOf(d.Enabled, "yes", "no", "unknown") || !routingcontext.ValidIdentifier(d.Source) || !routingcontext.ValidIdentifier(d.SourceVersion) ||
			!interval(d.ObservedAt, d.EffectiveFrom, d.ExpiresAt) || d.ObservedAt.After(artifact.PublishedAt) {
			return nil, errors.New("capability descriptor has invalid support, provenance or interval")
		}
		if !oneOf(d.CountryField, "merchant.country", "payment_method_context.issuer_country") {
			return nil, errors.New("country constraint must name one canonical country field")
		}
		sets := []struct {
			value   *Set
			allowed func(string) bool
		}{
			{&d.Currencies, currencyCode.MatchString}, {&d.Countries, countryCode.MatchString},
			{&d.PaymentMethods, func(v string) bool { return v == "card" }},
			{&d.Operations, func(v string) bool { return oneOf(v, "sale", "authorize") }},
			{&d.CaptureMethods, func(v string) bool { return oneOf(v, "automatic", "manual") }},
			{&d.CredentialDomains, routingcontext.ValidIdentifier},
		}
		for _, set := range sets {
			if err := normalizeSet(set.value, set.allowed); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(artifact.Candidates, func(i, j int) bool { return artifact.Candidates[i].ID < artifact.Candidates[j].ID })
	sort.Slice(artifact.Descriptors, func(i, j int) bool { return artifact.Descriptors[i].CandidateID < artifact.Descriptors[j].CandidateID })
	encoded, _ = json.Marshal(artifact)
	hash := sha256.Sum256(encoded)
	return &Catalog{artifact: artifact, digest: "sha256:" + hex.EncodeToString(hash[:])}, nil
}

func normalizeSet(set *Set, allowed func(string) bool) error {
	if !oneOf(set.State, "known", "unknown") || len(set.Values) > 256 || (set.State == "unknown" && len(set.Values) != 0) {
		return errors.New("capability set must explicitly be known or unknown")
	}
	seen := make(map[string]bool)
	for _, value := range set.Values {
		if !allowed(value) || seen[value] {
			return errors.New("capability set contains an invalid or duplicate value")
		}
		seen[value] = true
	}
	sort.Strings(set.Values)
	return nil
}

func (c *Catalog) Version() string { return c.artifact.Version }
func (c *Catalog) Digest() string  { return c.digest }
func (c *Catalog) Candidates() []routing.CandidateRef {
	return append([]routing.CandidateRef(nil), c.artifact.Candidates...)
}
func (c *Catalog) CandidateIDs() []string {
	result := make([]string, 0, len(c.artifact.Candidates))
	for _, candidate := range c.artifact.Candidates {
		result = append(result, candidate.ID)
	}
	return result
}
func (c *Catalog) ValidAt(at time.Time) bool {
	return !at.Before(c.artifact.EffectiveFrom) && !at.Before(c.artifact.PublishedAt) && at.Before(c.artifact.ExpiresAt)
}

// Only positively selected candidates need this extra online check. A negative
// or unknown descriptor remains excluded; it cannot be resurrected by expiry.
func (c *Catalog) CandidatesValidAt(ids []string, at time.Time) bool {
	valid := make(map[string]bool, len(c.artifact.Descriptors))
	for _, descriptor := range c.artifact.Descriptors {
		valid[descriptor.CandidateID] = !at.Before(descriptor.ObservedAt) && !at.Before(descriptor.EffectiveFrom) && at.Before(descriptor.ExpiresAt)
	}
	for _, id := range ids {
		if !valid[id] {
			return false
		}
	}
	return true
}

func (c *Catalog) Filter(context routingcontext.Context) []Result {
	results := make([]Result, 0, len(c.artifact.Descriptors))
	for _, descriptor := range c.artifact.Descriptors {
		result := Result{CandidateID: descriptor.CandidateID, Status: Eligible, Reasons: []routing.Reason{}}
		add := func(state, code, detail string) {
			if state == Ineligible || result.Status == Eligible {
				result.Status = state
			}
			result.Reasons = append(result.Reasons, routing.Reason{Code: code, Detail: detail})
		}
		at := context.BuiltAt
		if !c.ValidAt(at) {
			add(Unknown, "capability_snapshot_stale", "catalog is outside its published effective interval")
		}
		if at.Before(descriptor.ObservedAt) || at.Before(descriptor.EffectiveFrom) || !at.Before(descriptor.ExpiresAt) {
			add(Unknown, "capability_snapshot_stale", "descriptor is outside its observed effective interval")
		}
		// Expired support/no values are evidence of the past, not known current
		// facts. Never produce a positive or negative capability claim from them.
		if result.Status == Unknown {
			results = append(results, result)
			continue
		}
		switch descriptor.Enabled {
		case "no":
			add(Ineligible, routing.ReasonProviderDisabled, "connection is explicitly disabled")
		case "unknown":
			add(Unknown, "capability_unknown", "connection enabled state is unknown")
		}
		checks := []struct {
			dimension string
			set       Set
			actual    routingcontext.Value
			noCode    string
		}{
			{"currency", descriptor.Currencies, context.Lookup("base.currency"), routing.ReasonCurrencyNotSupported},
			{"country", descriptor.Countries, context.Lookup(descriptor.CountryField), "country_not_supported"},
			{"payment_method", descriptor.PaymentMethods, context.Lookup("base.payment_method"), routing.ReasonPaymentMethodNotSupported},
			{"operation", descriptor.Operations, context.Lookup("base.operation"), routing.ReasonOperationNotSupported},
			{"capture_method", descriptor.CaptureMethods, context.Lookup("base.capture_method"), "capture_method_not_supported"},
			{"credential_domain", descriptor.CredentialDomains, context.Lookup("payment_method_context.credential_domain"), "token_not_portable"},
		}
		for _, check := range checks {
			if check.set.State != "known" || check.actual.State != routingcontext.StateKnown {
				code := "capability_unknown"
				if check.dimension == "credential_domain" && check.actual.State != routingcontext.StateKnown {
					code = "token_binding_unknown"
				}
				add(Unknown, code, fmt.Sprintf("%s support or required context fact is unknown", check.dimension))
			} else if !oneOf(check.actual.String, check.set.Values...) {
				add(Ineligible, check.noCode, fmt.Sprintf("connection does not support the requested %s", check.dimension))
			}
		}
		results = append(results, result)
	}
	return results
}

func interval(observed, effective, expires time.Time) bool {
	_, a := observed.Zone()
	_, b := effective.Zone()
	_, c := expires.Zone()
	return !observed.IsZero() && !effective.IsZero() && !expires.IsZero() && a == 0 && b == 0 && c == 0 && expires.After(effective) && expires.After(observed)
}
func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}
