// Package routingcontext is Router's credential-free, versioned input contract.
// Its wire field order intentionally matches Payment's independent boundary
// type; a shared golden test protects the full SHA-256 Context digest.
package routingcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const SchemaVersion = "routing_context.v1"

const (
	StateKnown         = "known"
	StateUnknown       = "unknown"
	StateNotApplicable = "not_applicable"
	StateRedacted      = "redacted"
	StateStale         = "stale"
)

var ErrInvalidContext = errors.New("invalid routing context")

// ValidationError never contains caller-supplied values or unknown field names.
type ValidationError struct{ Field, Message string }

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }
func (e *ValidationError) Unwrap() error { return ErrInvalidContext }

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

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var country = regexp.MustCompile(`^[A-Z]{2}$`)
var currency = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidIdentifier is for non-secret Context/provenance identifiers, not for
// connection IDs (which retain their stricter existing lowercase contract).
func ValidIdentifier(value string) bool { return identifier.MatchString(value) }

func (c Context) Validate() error {
	bad := func(field, message string) error { return &ValidationError{Field: field, Message: message} }
	if c.SchemaVersion != SchemaVersion {
		return bad("context_schema_version", "unsupported schema version")
	}
	if !utcTime(c.BuiltAt) {
		return bad("built_at", "must be a nonzero UTC instant")
	}
	for _, field := range []struct{ name, value string }{
		{"base.payment_id", c.Base.PaymentID}, {"base.attempt_id", c.Base.AttemptID},
		{"base.merchant_id", c.Base.MerchantID}, {"merchant.entity_id", c.Merchant.EntityID},
	} {
		if !ValidIdentifier(field.value) {
			return bad(field.name, "must be a bounded canonical identifier")
		}
	}
	if c.Base.AmountMinor <= 0 {
		return bad("base.amount_minor", "must be a positive integer in minor units")
	}
	if !currency.MatchString(c.Base.Currency) {
		return bad("base.currency", "must be a three-letter uppercase currency code")
	}
	if c.Base.PaymentMethod != "card" || c.PaymentMethod.Category != c.Base.PaymentMethod {
		return bad("payment_method_context.category", "must equal the supported base payment method")
	}
	if !((c.Base.Operation == "sale" && c.Base.CaptureMethod == "automatic") ||
		(c.Base.Operation == "authorize" && c.Base.CaptureMethod == "manual")) {
		return bad("base.capture_method", "must match sale/automatic or authorize/manual; no follow-on operation may be routed")
	}
	for _, field := range c.observations() {
		if err := validateObserved(field.value, c.BuiltAt); err != nil {
			return bad(field.name, "invalid state, value, provenance or validity interval")
		}
	}
	if c.Merchant.Country.State != StateKnown || !country.MatchString(*c.Merchant.Country.Value) {
		return bad("merchant.country", "a known canonical merchant country is required")
	}
	if value := c.PaymentMethod.IssuerCountry.Value; value != nil && !country.MatchString(*value) {
		return bad("payment_method_context.issuer_country", "must be a two-letter uppercase country code")
	}
	if value := c.PaymentMethod.Network.Value; value != nil && !oneOf(*value, "visa", "mastercard", "amex", "discover", "jcb", "unionpay", "diners") {
		return bad("payment_method_context.network", "unsupported network metadata")
	}
	return nil
}

// ValidateOnline is an admission gate, separate from deterministic evaluation
// at BuiltAt. Replaying a once-valid but now expired snapshot cannot extend its
// authority; successful evaluation itself never reads a wall clock.
func (c Context) ValidateOnline(now time.Time, maxAge time.Duration) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if maxAge <= 0 || now.IsZero() || c.BuiltAt.After(now) || now.Sub(c.BuiltAt) > maxAge {
		return &ValidationError{Field: "built_at", Message: "snapshot is from the future or outside the online freshness window"}
	}
	for _, field := range c.observations() {
		if field.value.State == StateKnown && !now.Before(*field.value.ExpiresAt) {
			return &ValidationError{Field: field.name, Message: "known observation expired before online admission"}
		}
	}
	return nil
}

func validateObserved(field Observed, builtAt time.Time) error {
	if !ValidIdentifier(field.Source) || !ValidIdentifier(field.SourceVersion) || !utcTime(field.ObservedAt) || field.ObservedAt.After(builtAt) {
		return ErrInvalidContext
	}
	if field.ExpiresAt != nil && (!utcTime(*field.ExpiresAt) || !field.ExpiresAt.After(field.ObservedAt)) {
		return ErrInvalidContext
	}
	switch field.State {
	case StateKnown, StateStale:
		if field.Value == nil || !ValidIdentifier(*field.Value) || field.ExpiresAt == nil ||
			(field.State == StateKnown && !builtAt.Before(*field.ExpiresAt)) ||
			(field.State == StateStale && builtAt.Before(*field.ExpiresAt)) {
			return ErrInvalidContext
		}
	case StateUnknown, StateNotApplicable, StateRedacted:
		if field.Value != nil {
			return ErrInvalidContext
		}
	default:
		return ErrInvalidContext
	}
	return nil
}

type observation struct {
	name  string
	value Observed
}

func (c Context) observations() []observation {
	return []observation{
		{"merchant.country", c.Merchant.Country}, {"payment_method_context.network", c.PaymentMethod.Network},
		{"payment_method_context.issuer_country", c.PaymentMethod.IssuerCountry},
		{"payment_method_context.credential_domain", c.PaymentMethod.CredentialDomain},
	}
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
	return Observed{State: StateKnown, Value: &value, Source: source, SourceVersion: version, ObservedAt: observedAt, ExpiresAt: &expiresAt}
}
func Unknown(source, version string, observedAt time.Time) Observed {
	return Observed{State: StateUnknown, Source: source, SourceVersion: version, ObservedAt: observedAt.UTC()}
}
func utcTime(value time.Time) bool { _, offset := value.Zone(); return !value.IsZero() && offset == 0 }
func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if option == value {
			return true
		}
	}
	return false
}
