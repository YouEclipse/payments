// Package routingcontext builds the minimal, non-credential snapshot consumed
// by Router v2. The payment token is an input to metadata lookup, never a field
// of the snapshot, its digest, an error message or a routing decision.
package routingcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = "routing_context.v1"

var ErrInvalidContext = errors.New("invalid routing context")

type Observed struct {
	State         string     `json:"state"`
	Value         *string    `json:"value,omitempty"`
	Source        string     `json:"source"`
	SourceVersion string     `json:"source_version"`
	ObservedAt    time.Time  `json:"observed_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

type Base struct {
	PaymentID     string `json:"payment_id"`
	AttemptID     string `json:"attempt_id"`
	MerchantID    string `json:"merchant_id"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	PaymentMethod string `json:"payment_method"`
	Operation     string `json:"operation"`
	CaptureMethod string `json:"capture_method"`
}

type Merchant struct {
	EntityID string   `json:"entity_id"`
	Country  Observed `json:"country"`
}

type PaymentMethod struct {
	Category         string   `json:"category"`
	Network          Observed `json:"network"`
	IssuerCountry    Observed `json:"issuer_country"`
	CredentialDomain Observed `json:"credential_domain"`
}

type Context struct {
	SchemaVersion string        `json:"context_schema_version"`
	BuiltAt       time.Time     `json:"built_at"`
	Base          Base          `json:"base"`
	Merchant      Merchant      `json:"merchant"`
	PaymentMethod PaymentMethod `json:"payment_method_context"`
}

type MerchantProfile struct {
	MerchantID string    `json:"merchant_id"`
	EntityID   string    `json:"entity_id"`
	Country    string    `json:"country"`
	Version    string    `json:"version"`
	ObservedAt time.Time `json:"observed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Metadata struct {
	Network          Observed
	IssuerCountry    Observed
	CredentialDomain Observed
}

type MetadataSource interface {
	Lookup(context.Context, string, time.Time) (Metadata, error)
}

type Input struct {
	PaymentID          string
	AttemptID          string
	MerchantID         string
	AmountMinor        int64
	Currency           string
	CaptureMethod      string
	PaymentMethodToken string
}

type Builder struct {
	profiles map[string]MerchantProfile
	metadata MetadataSource
}

func NewBuilder(profiles []MerchantProfile, metadata MetadataSource) (*Builder, error) {
	if len(profiles) == 0 || metadata == nil {
		return nil, fmt.Errorf("%w: merchant profiles and metadata source are required", ErrInvalidContext)
	}
	result := &Builder{profiles: make(map[string]MerchantProfile, len(profiles)), metadata: metadata}
	for _, profile := range profiles {
		if !identifier.MatchString(profile.MerchantID) || !identifier.MatchString(profile.EntityID) ||
			!country.MatchString(profile.Country) || !identifier.MatchString(profile.Version) ||
			profile.ObservedAt.IsZero() || !profile.ExpiresAt.After(profile.ObservedAt) {
			return nil, fmt.Errorf("%w: invalid merchant profile", ErrInvalidContext)
		}
		if _, exists := result.profiles[profile.MerchantID]; exists {
			return nil, fmt.Errorf("%w: duplicate merchant profile", ErrInvalidContext)
		}
		profile.ObservedAt, profile.ExpiresAt = profile.ObservedAt.UTC(), profile.ExpiresAt.UTC()
		result.profiles[profile.MerchantID] = profile
	}
	return result, nil
}

func (b *Builder) Build(ctx context.Context, input Input, now time.Time) (Context, error) {
	now = now.UTC()
	profile, found := b.profiles[input.MerchantID]
	if !found {
		return Context{}, fmt.Errorf("%w: merchant profile unavailable", ErrInvalidContext)
	}
	if input.CaptureMethod != "automatic" {
		return Context{}, fmt.Errorf("%w: only automatic SALE is implemented", ErrInvalidContext)
	}
	metadata, err := b.metadata.Lookup(ctx, input.PaymentMethodToken, now)
	if err != nil {
		// A resolver error can contain a credential or an upstream URL. Do not
		// propagate it through the public API or structured logs.
		return Context{}, fmt.Errorf("%w: payment method metadata unavailable", ErrInvalidContext)
	}
	result := Context{
		SchemaVersion: SchemaVersion, BuiltAt: now,
		Base: Base{PaymentID: input.PaymentID, AttemptID: input.AttemptID, MerchantID: input.MerchantID,
			AmountMinor: input.AmountMinor, Currency: input.Currency, PaymentMethod: "card", Operation: "sale", CaptureMethod: "automatic"},
		Merchant: Merchant{EntityID: profile.EntityID, Country: Known(profile.Country, "merchant_config", profile.Version, profile.ObservedAt, profile.ExpiresAt)},
		PaymentMethod: PaymentMethod{Category: "card", Network: cloneObserved(metadata.Network),
			IssuerCountry: cloneObserved(metadata.IssuerCountry), CredentialDomain: cloneObserved(metadata.CredentialDomain)},
	}
	if err := result.Validate(); err != nil {
		return Context{}, err
	}
	return result, nil
}

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	country    = regexp.MustCompile(`^[A-Z]{2}$`)
	currency   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Validate checks the minimal wire contract. Country/currency membership and
// account support remain the versioned capability catalog's responsibility;
// these syntax checks do not claim to implement all ISO reference data.
func (c Context) Validate() error {
	if c.SchemaVersion != SchemaVersion || c.BuiltAt.IsZero() ||
		!identifier.MatchString(c.Base.PaymentID) || !identifier.MatchString(c.Base.AttemptID) ||
		!identifier.MatchString(c.Base.MerchantID) || !identifier.MatchString(c.Merchant.EntityID) ||
		c.Base.AmountMinor <= 0 || !currency.MatchString(c.Base.Currency) ||
		c.Base.PaymentMethod != "card" || c.PaymentMethod.Category != c.Base.PaymentMethod ||
		c.Base.Operation != "sale" || c.Base.CaptureMethod != "automatic" {
		return fmt.Errorf("%w: invalid base or schema", ErrInvalidContext)
	}
	fields := []struct {
		name  string
		value Observed
	}{
		{"merchant.country", c.Merchant.Country}, {"payment_method_context.network", c.PaymentMethod.Network},
		{"payment_method_context.issuer_country", c.PaymentMethod.IssuerCountry},
		{"payment_method_context.credential_domain", c.PaymentMethod.CredentialDomain},
	}
	for _, field := range fields {
		if err := validateObserved(field.value, c.BuiltAt); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidContext, field.name)
		}
	}
	if c.Merchant.Country.State != "known" || !country.MatchString(*c.Merchant.Country.Value) {
		return fmt.Errorf("%w: known merchant country required", ErrInvalidContext)
	}
	if value := c.PaymentMethod.IssuerCountry.Value; value != nil && !country.MatchString(*value) {
		return fmt.Errorf("%w: issuer country format", ErrInvalidContext)
	}
	if value := c.PaymentMethod.Network.Value; value != nil && !oneOf(*value, "visa", "mastercard", "amex", "discover", "jcb", "unionpay", "diners") {
		return fmt.Errorf("%w: unsupported network metadata", ErrInvalidContext)
	}
	return nil
}

func validateObserved(field Observed, builtAt time.Time) error {
	if !identifier.MatchString(field.Source) || !identifier.MatchString(field.SourceVersion) ||
		field.ObservedAt.IsZero() || field.ObservedAt.After(builtAt) {
		return ErrInvalidContext
	}
	switch field.State {
	case "known", "stale":
		if field.Value == nil || !identifier.MatchString(*field.Value) || field.ExpiresAt == nil ||
			!field.ExpiresAt.After(field.ObservedAt) || (field.State == "known" && !builtAt.Before(*field.ExpiresAt)) ||
			(field.State == "stale" && builtAt.Before(*field.ExpiresAt)) {
			return ErrInvalidContext
		}
	case "unknown", "not_applicable", "redacted":
		if field.Value != nil || (field.ExpiresAt != nil && !field.ExpiresAt.After(field.ObservedAt)) {
			return ErrInvalidContext
		}
	default:
		return ErrInvalidContext
	}
	return nil
}

func (c Context) Digest() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func Known(value, source, version string, observedAt, expiresAt time.Time) Observed {
	observedAt, expiresAt = observedAt.UTC(), expiresAt.UTC()
	return Observed{State: "known", Value: &value, Source: source, SourceVersion: version, ObservedAt: observedAt, ExpiresAt: &expiresAt}
}

func Unknown(source, version string, observedAt time.Time) Observed {
	return Observed{State: "unknown", Source: source, SourceVersion: version, ObservedAt: observedAt.UTC()}
}

func cloneObserved(value Observed) Observed {
	value.ObservedAt = value.ObservedAt.UTC()
	if value.Value != nil {
		copied := *value.Value
		value.Value = &copied
	}
	if value.ExpiresAt != nil {
		copied := value.ExpiresAt.UTC()
		value.ExpiresAt = &copied
	}
	return value
}

func oneOf(value string, allowed ...string) bool {
	for _, option := range allowed {
		if value == option {
			return true
		}
	}
	return false
}

// SimulatorMetadata is an explicit dev/test adapter, not a token portability
// inference rule. A live integration must use its vault/PSP metadata owner.
type SimulatorMetadata struct{}

func (SimulatorMetadata) Lookup(ctx context.Context, token string, now time.Time) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if !oneOf(token, "pm_sim_approved", "pm_sim_declined", "pm_sim_timeout_then_succeeded", "pm_sim_timeout_then_declined") {
		return Metadata{}, errors.New("unsupported simulator token")
	}
	unknown := Unknown("simulator_metadata", "simulator_metadata.v1", now)
	return Metadata{Network: unknown, IssuerCountry: unknown,
		CredentialDomain: Known("simulator_a", "simulator_metadata", "simulator_metadata.v1", now, now.Add(5*time.Minute))}, nil
}

// UnavailableMetadata exposes missing trusted metadata as unknown. It never
// guesses credential compatibility from a token string in non-local setups.
type UnavailableMetadata struct{}

func (UnavailableMetadata) Lookup(ctx context.Context, _ string, now time.Time) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	unknown := Unknown("metadata_unavailable", "metadata_unavailable.v1", now)
	return Metadata{Network: unknown, IssuerCountry: unknown, CredentialDomain: unknown}, nil
}

func IsLocalEnvironment(environment string) bool {
	return oneOf(strings.ToLower(strings.TrimSpace(environment)), "local", "dev", "development", "test")
}
