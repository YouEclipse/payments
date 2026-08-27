package scoring

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCompileRejectsAmbiguousOrUnsafeProfiles(t *testing.T) {
	for name, edit := range map[string]func(*Profile){
		"schema":              func(p *Profile) { p.SchemaVersion = "scoring_profile.v9" },
		"empty version":       func(p *Profile) { p.Version = "" },
		"long version":        func(p *Profile) { p.Version = strings.Repeat("a", 129) },
		"unsafe version":      func(p *Profile) { p.Version = "profile/new" },
		"implicit primary":    func(p *Profile) { p.Primary = "" },
		"unknown primary":     func(p *Profile) { p.Primary = "expected_utility" },
		"implicit fallback":   func(p *Profile) { p.OnUnavailable = "" },
		"per candidate mode":  func(p *Profile) { p.OnUnavailable = "candidate_default" },
		"currency lower case": func(p *Profile) { p.QuoteCurrency = "usd" },
		"empty currency":      func(p *Profile) { p.QuoteCurrency = "" },
		"negative weight":     func(p *Profile) { p.SuccessWeightBPS = -1 },
		"too large weight":    func(p *Profile) { p.CostWeightBPS = PointScale + 1 },
		"weights not total":   func(p *Profile) { p.CostWeightBPS++ },
		"negative floor":      func(p *Profile) { p.CostFloorMicros = -1 },
		"equal bounds":        func(p *Profile) { p.CostCeilingMicros = p.CostFloorMicros },
		"reversed bounds":     func(p *Profile) { p.CostCeilingMicros = p.CostFloorMicros - 1 },
		"missing publication": func(p *Profile) { p.PublishedAt = time.Time{} },
		"missing effective":   func(p *Profile) { p.EffectiveFrom = time.Time{} },
		"missing expiry":      func(p *Profile) { p.ExpiresAt = time.Time{} },
		"publication late":    func(p *Profile) { p.PublishedAt = p.EffectiveFrom.Add(time.Nanosecond) },
		"empty lifetime":      func(p *Profile) { p.ExpiresAt = p.EffectiveFrom },
		"non UTC":             func(p *Profile) { p.PublishedAt = p.PublishedAt.In(time.FixedZone("local", 3600)) },
		"unsupported year":    func(p *Profile) { p.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		t.Run(name, func(t *testing.T) {
			profile, _, _, _ := scoringFixture()
			edit(&profile)
			got, err := Compile(profile)
			if !errors.Is(err, ErrInvalidProfile) || got != nil {
				t.Fatal("invalid profile compiled", got, err)
			}
		})
	}
}

func TestProfileDigestBindsEveryBehavioralChoice(t *testing.T) {
	profile, _, _, _ := scoringFixture()
	original := mustCompile(t, profile)
	if !digestValue.MatchString(original.Digest()) || original.Version() != profile.Version {
		t.Fatal("missing profile identity")
	}
	for name, edit := range map[string]func(*Profile){
		"version":     func(p *Profile) { p.Version = "score_v2" },
		"publication": func(p *Profile) { p.PublishedAt = p.PublishedAt.Add(-time.Nanosecond) },
		"effective":   func(p *Profile) { p.EffectiveFrom = p.EffectiveFrom.Add(time.Nanosecond) },
		"expiry":      func(p *Profile) { p.ExpiresAt = p.ExpiresAt.Add(time.Nanosecond) },
		"currency":    func(p *Profile) { p.QuoteCurrency = "EUR" },
		"weights":     func(p *Profile) { p.SuccessWeightBPS--; p.CostWeightBPS++ },
		"floor":       func(p *Profile) { p.CostFloorMicros++ },
		"ceiling":     func(p *Profile) { p.CostCeilingMicros++ },
		"fallback":    func(p *Profile) { p.OnUnavailable = StaticOrder },
	} {
		t.Run(name, func(t *testing.T) {
			changed := profile
			edit(&changed)
			if mustCompile(t, changed).Digest() == original.Digest() {
				t.Fatal("behavior changed without a new digest")
			}
		})
	}
	profile.PublishedAt = profile.PublishedAt.In(time.FixedZone("equivalent_utc", 0))
	if mustCompile(t, profile).Digest() != original.Digest() {
		t.Fatal("equivalent UTC representation changed canonical digest")
	}
	profile.CostFloorMicros, profile.CostCeilingMicros = 0, math.MaxInt64
	profile.SuccessWeightBPS, profile.CostWeightBPS = 0, PointScale
	if _, err := Compile(profile); err != nil {
		t.Fatal("exact int64-wide normalization unnecessarily rejected", err)
	}
}

func TestValidityIsExplicitHalfOpenAndCannotFallBack(t *testing.T) {
	profile, fees, perf, at := scoringFixture()
	profile.OnUnavailable = StaticOrder
	scorer := mustCompile(t, profile)
	for _, tc := range []struct {
		at    time.Time
		valid bool
	}{
		{profile.PublishedAt.Add(-time.Nanosecond), false},
		{profile.PublishedAt, false},
		{profile.EffectiveFrom.Add(-time.Nanosecond), false},
		{profile.EffectiveFrom, true},
		{profile.ExpiresAt.Add(-time.Nanosecond), true},
		{profile.ExpiresAt, false},
		{time.Time{}, false},
		{at.In(time.FixedZone("offset", 3600)), false},
	} {
		if got := scorer.ValidAt(tc.at); got != tc.valid {
			t.Fatalf("ValidAt(%s)=%v want %v", tc.at, got, tc.valid)
		}
		if !tc.valid {
			result, err := scorer.Score(tc.at, Batch{})
			if !errors.Is(err, ErrProfileNotEffective) || result.Status != StatusRejected || len(result.CandidateOrder) != 0 || result.Mode != Reject {
				t.Fatal("inactive profile escaped into static fallback", result, err)
			}
		}
	}
	for _, value := range []*Scorer{nil, {}} {
		if value.Version() != "" || value.Digest() != "" || value.ValidAt(at) {
			t.Fatal("uncompiled scorer exposed identity or validity")
		}
		result, err := value.Score(at, evaluatedBatch(t, fees, perf, at, "USD"))
		if !errors.Is(err, ErrInvalidProfile) || len(result.CandidateOrder) != 0 || result.Status != StatusRejected {
			t.Fatal("uncompiled scorer produced a candidate", result, err)
		}
	}
}
