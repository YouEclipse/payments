package simulator

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSaleIsIdempotent(t *testing.T) {
	server, err := New(Config{Environment: "test", ProviderID: "simulator_a", WebhookSecret: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	body := SaleRequest{
		OperationID:        "op_123",
		AmountMinor:        5200,
		Currency:           "USD",
		PaymentMethodToken: "pm_sim_approved",
		CaptureMethod:      "automatic",
	}
	first := saleRequest(t, ts.URL, "same-key", body)
	second := saleRequest(t, ts.URL, "same-key", body)
	if first.ProviderReference == "" || first.ProviderReference != second.ProviderReference {
		t.Fatalf("expected the same provider side effect, first=%+v second=%+v", first, second)
	}
	if first.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", first.Status)
	}
}

func TestIdempotencyKeyRejectsDifferentPayload(t *testing.T) {
	server, err := New(Config{Environment: "test", ProviderID: "simulator_a", WebhookSecret: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	body := SaleRequest{OperationID: "op_123", AmountMinor: 5200, Currency: "USD", PaymentMethodToken: "pm_sim_approved", CaptureMethod: "automatic"}
	_ = saleRequest(t, ts.URL, "same-key", body)
	body.AmountMinor++
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sales", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "same-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestSimulatorRejectsLiveEnvironment(t *testing.T) {
	for _, environment := range []string{"live", "Production", "prod", "staging", "sandbox"} {
		if _, err := New(Config{Environment: environment, ProviderID: "simulator_a", WebhookSecret: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			t.Fatalf("expected %q environment to be rejected", environment)
		}
	}
}

func TestSimulatorNormalizesDevelopmentEnvironment(t *testing.T) {
	server, err := New(Config{Environment: " DEV ", ProviderID: "simulator_a", WebhookSecret: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if server.cfg.Environment != "development" {
		t.Fatalf("normalized environment = %q, want development", server.cfg.Environment)
	}
}

func saleRequest(t *testing.T, baseURL, key string, body SaleRequest) SaleResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/sales", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, payload)
	}
	var result SaleResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
