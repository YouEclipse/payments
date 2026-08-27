package routingcontext

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestRiskWireGoldenAndOnlineExpiry(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/internal/fixtures/routing-context-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var context Context
	if err := json.Unmarshal(raw, &context); err != nil {
		t.Fatal(err)
	}
	if err := context.ValidateOnline(context.BuiltAt, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(context)
	if !bytes.Equal(bytes.TrimSpace(raw), encoded) {
		t.Fatal("wire type diverged from Payment")
	}
	if got, err := context.Digest(); err != nil || got != "sha256:6270dd55213d0364a6300c425b6780b679ae6ec73fbb40155536146619ec8d8f" {
		t.Fatalf("digest=%s %v", got, err)
	}
	if got, err := context.RiskSubjectDigest(); err != nil || got != "sha256:17f9f984922f727489962662550f75dfca023da86e9acc7a6f99e5d6ba5c20ca" {
		t.Fatalf("subject=%s %v", got, err)
	}
	context.Risk.ExpiresAt = context.BuiltAt.Add(time.Second)
	if err := context.ValidateOnline(context.Risk.ExpiresAt, 5*time.Minute); err == nil {
		t.Fatal("expired risk ALLOW remained executable")
	}
	context.Risk.ExpiresAt = context.BuiltAt.Add(time.Minute)
	context.Base.AttemptID = "another_attempt"
	if err := context.Validate(); err == nil {
		t.Fatal("ALLOW rebound to another Attempt")
	}
}
