// Package riskdecision defines a credential-free contract, not a fraud engine.
// Payment obtains its facts from an authorized source; Router only enforces
// the action. Shared validation prevents different service interpretations.
package riskdecision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"
)

const SchemaVersion = "risk_decision.v1"

const (
	Allow        = "allow"
	Block        = "block"
	Require3DS   = "require_3ds"
	ManualReview = "manual_review"
)

var ErrInvalid = errors.New("invalid risk decision")

type Decision struct {
	SchemaVersion string    `json:"schema_version"`
	DecisionID    string    `json:"decision_id"`
	SubjectDigest string    `json:"subject_digest"`
	Action        string    `json:"action"`
	ReasonCodes   []string  `json:"reason_codes"`
	Version       string    `json:"version"`
	Source        string    `json:"source"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// Evidence describes enforcement of the original decision. It carries no raw
// fraud features or arbitrary provider text. The current funds slice does not
// implement authentication, so require_3ds is explicitly non-executable.
type Evidence struct {
	DecisionID           string   `json:"decision_id"`
	DecisionDigest       string   `json:"decision_digest"`
	Version              string   `json:"version"`
	Action               string   `json:"action"`
	Outcome              string   `json:"outcome"`
	Continue             bool     `json:"continue"`
	ReasonCodes          []string `json:"reason_codes"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var reasonCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func ValidDigest(value string) bool {
	if len(value) != 71 || value[:7] != "sha256:" {
		return false
	}
	for _, c := range value[7:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (d Decision) Validate(at time.Time, subjectDigest string) error {
	if d.SchemaVersion != SchemaVersion || !identifier.MatchString(d.DecisionID) ||
		!identifier.MatchString(d.Version) || !identifier.MatchString(d.Source) ||
		!ValidDigest(subjectDigest) || d.SubjectDigest != subjectDigest ||
		!utc(d.EvaluatedAt) || !utc(d.ExpiresAt) || at.IsZero() || d.EvaluatedAt.After(at) ||
		!d.ExpiresAt.After(d.EvaluatedAt) || !at.Before(d.ExpiresAt) ||
		d.ReasonCodes == nil || len(d.ReasonCodes) > 32 || !slices.IsSorted(d.ReasonCodes) {
		return ErrInvalid
	}
	if d.Action != Allow && d.Action != Block && d.Action != Require3DS && d.Action != ManualReview {
		return ErrInvalid
	}
	if d.Action != Allow && len(d.ReasonCodes) == 0 {
		return ErrInvalid
	}
	for i, reason := range d.ReasonCodes {
		if !reasonCode.MatchString(reason) || (i > 0 && d.ReasonCodes[i-1] == reason) {
			return ErrInvalid
		}
	}
	return nil
}

func EvaluateSale(at time.Time, subjectDigest string, d Decision) (Evidence, error) {
	if err := d.Validate(at, subjectDigest); err != nil {
		return Evidence{}, err
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return Evidence{}, ErrInvalid
	}
	digest := sha256.Sum256(payload)
	result := Evidence{DecisionID: d.DecisionID, DecisionDigest: "sha256:" + hex.EncodeToString(digest[:]),
		Version: d.Version, Action: d.Action, ReasonCodes: slices.Clone(d.ReasonCodes), RequiredCapabilities: []string{}}
	switch d.Action {
	case Allow:
		result.Continue, result.Outcome = true, "allowed"
	case Block:
		result.Outcome = "blocked"
	case ManualReview:
		result.Outcome = "requires_review"
	case Require3DS:
		result.Outcome = "authentication_unsupported"
		result.RequiredCapabilities = []string{"3ds_required"}
	}
	return result, nil
}

func Clone(d Decision) Decision { d.ReasonCodes = slices.Clone(d.ReasonCodes); return d }

func utc(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}
