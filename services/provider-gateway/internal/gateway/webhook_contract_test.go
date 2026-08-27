package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestWebhookOccurredAtIsRequiredByWireContractAndRuntime(t *testing.T) {
	contract, err := os.ReadFile("../../../../contracts/internal/provider-gateway-api.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	requiredLine := regexp.MustCompile(`(?m)^\s*required:\s*\[event_id, provider, operation_id, status, occurred_at\]\s*$`)
	if !requiredLine.Match(contract) {
		t.Fatal("ProviderWebhookEvent OpenAPI contract does not require occurred_at")
	}

	service := newTestService(t, NewMemoryStore(), map[string]string{"provider_a": "http://provider.test"})
	body := []byte(`{"event_id":"evt_missing_time","provider":"provider_a","operation_id":"op_missing_time","status":"captured"}`)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write(body)
	_, err = service.ApplyWebhook(t.Context(), "provider_a", body, hex.EncodeToString(mac.Sum(nil)))
	if !errors.Is(err, ErrInvalidWebhook) || !strings.Contains(err.Error(), "identity is incomplete") {
		t.Fatalf("webhook without occurred_at error=%v, want ErrInvalidWebhook incomplete identity", err)
	}
}
