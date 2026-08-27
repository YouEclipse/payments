package scoring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
)

var (
	identifier   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	candidateID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)
	digestValue  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reasonCode   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	countryCode  = regexp.MustCompile(`^[A-Z]{2}$`)
)

type Scorer struct {
	profile Profile
	digest  string
}

func Compile(input Profile) (*Scorer, error) {
	if input.SchemaVersion != SchemaVersion || !identifier.MatchString(input.Version) ||
		input.Primary != PrimaryFull || (input.OnUnavailable != StaticOrder && input.OnUnavailable != Reject) ||
		!currencyCode.MatchString(input.QuoteCurrency) ||
		input.SuccessWeightBPS < 0 || input.SuccessWeightBPS > PointScale || input.CostWeightBPS < 0 || input.CostWeightBPS > PointScale ||
		input.SuccessWeightBPS+input.CostWeightBPS != PointScale || input.CostFloorMicros < 0 || input.CostCeilingMicros <= input.CostFloorMicros ||
		!utcInstant(input.PublishedAt) || !utcInstant(input.EffectiveFrom) || !utcInstant(input.ExpiresAt) ||
		input.PublishedAt.After(input.EffectiveFrom) || !input.EffectiveFrom.Before(input.ExpiresAt) {
		return nil, ErrInvalidProfile
	}
	// Profile consists only of values; no caller-owned slice/map/pointer is
	// retained. UTC conversion also removes monotonic-clock process metadata.
	profile := input
	profile.PublishedAt, profile.EffectiveFrom, profile.ExpiresAt = input.PublishedAt.UTC(), input.EffectiveFrom.UTC(), input.ExpiresAt.UTC()
	canonical, err := json.Marshal(profile)
	if err != nil {
		return nil, ErrInvalidProfile
	}
	digest := sha256.Sum256(canonical)
	return &Scorer{profile: profile, digest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func (s *Scorer) Version() string {
	if s == nil {
		return ""
	}
	return s.profile.Version
}
func (s *Scorer) Digest() string {
	if s == nil {
		return ""
	}
	return s.digest
}
func (s *Scorer) ValidAt(at time.Time) bool {
	return s != nil && s.digest != "" && utcInstant(at) && !at.Before(s.profile.PublishedAt) && !at.Before(s.profile.EffectiveFrom) && at.Before(s.profile.ExpiresAt)
}

func utcInstant(at time.Time) bool {
	_, offset := at.Zone()
	return !at.IsZero() && offset == 0 && at.Year() >= 1 && at.Year() <= 9999
}
