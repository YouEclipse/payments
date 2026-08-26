package routingcontext

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictProfileArtifact(t *testing.T) {
	builder, _, _ := fixture(t)
	profile := builder.profiles["merchant_demo"]
	encoded, err := json.Marshal([]MerchantProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProfiles(string(encoded)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"null", "[]", string(encoded) + " {}",
		strings.Replace(string(encoded), `"country":"US"`, `"country":"US","country":"GB"`, 1),
		strings.Replace(string(encoded), `"country":"US"`, `"country":"US","Country":"GB"`, 1),
		strings.Replace(string(encoded), `"country":"US"`, `"Country":"US"`, 1),
		strings.Replace(string(encoded), `"merchant_id"`, `"MERCHANT_ID"`, 1),
		strings.Replace(string(encoded), `"entity_id"`, `"Entity_Id"`, 1),
		strings.Replace(string(encoded), `"version"`, `"Version"`, 1),
		strings.Replace(string(encoded), `"country":"US"`, `"country":"US","payment_method_token":"secret"`, 1),
		strings.Replace(string(encoded), `"merchant_demo"`, `""`, 1),
	} {
		if _, err := DecodeProfiles(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("expected sanitized rejection: %v", err)
		}
	}
}
