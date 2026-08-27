package routingcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
)

func TestRiskContextGoldenAndSubjectBinding(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/internal/fixtures/routing-context-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var got Context
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || !bytes.Equal(bytes.TrimSpace(raw), encoded) {
		t.Fatalf("noncanonical golden: %v", err)
	}
	digest, err := got.Digest()
	if err != nil || digest != "sha256:6270dd55213d0364a6300c425b6780b679ae6ec73fbb40155536146619ec8d8f" {
		t.Fatalf("golden digest: %s %v", digest, err)
	}
	subject, err := got.RiskSubjectDigest()
	if err != nil || subject != "sha256:17f9f984922f727489962662550f75dfca023da86e9acc7a6f99e5d6ba5c20ca" {
		t.Fatalf("subject digest: %s %v", subject, err)
	}
	changed := got
	changed.BuiltAt = got.BuiltAt.Add(time.Second)
	if next, _ := changed.RiskSubjectDigest(); next != subject {
		t.Fatal("envelope construction time changed business subject")
	}
	if next, _ := changed.Digest(); next == digest {
		t.Fatal("full Context failed to bind BuiltAt")
	}
	changed = got
	changed.Base.AmountMinor++
	if err := changed.Validate(); err == nil {
		t.Fatal("old ALLOW reused for a new amount")
	}
	changed = got
	changed.Risk = nil
	if err := changed.Validate(); err == nil {
		t.Fatal("Risk omitted from schema v2")
	}
	changed = got
	changed.SchemaVersion = SchemaVersion
	if err := changed.Validate(); err == nil {
		t.Fatal("Risk injected into legacy schema")
	}
}

func TestBuilderRiskIsExplicitAndCredentialFree(t *testing.T) {
	builder, input, at := fixture(t)
	withRisk, err := builder.WithRisk(DevelopmentRisk{})
	if err != nil {
		t.Fatal(err)
	}
	for amount, action := range map[int64]string{5200: riskdecision.Allow, 49001: riskdecision.Block, 49002: riskdecision.Require3DS, 49003: riskdecision.ManualReview} {
		input.AmountMinor = amount
		got, err := withRisk.Build(context.Background(), input, at)
		if err != nil || got.SchemaVersion != RiskSchemaVersion || got.Risk == nil || got.Risk.Action != action {
			t.Fatalf("%d: %+v %v", amount, got.Risk, err)
		}
	}
	legacy, err := builder.Build(context.Background(), input, at)
	if err != nil || legacy.SchemaVersion != SchemaVersion || legacy.Risk != nil {
		t.Fatal("adding a source mutated the legacy builder")
	}
	missing, _ := builder.WithRisk(UnavailableRisk{})
	if _, err := missing.Build(context.Background(), input, at); err == nil {
		t.Fatal("missing risk source became ALLOW")
	}
}
