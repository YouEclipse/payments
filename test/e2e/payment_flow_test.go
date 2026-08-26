//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type paymentIntent struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	IntegrityStatus string `json:"integrity_status"`
}

func TestPaymentVerticalSlice(t *testing.T) {
	baseURL := strings.TrimRight(env("PAYMENT_API_BASE_URL", "http://127.0.0.1:8080"), "/")
	apiKey := env("PAYMENT_TEST_API_KEY", "local-api-key")
	waitReady(t, baseURL)

	t.Run("approved sale and merchant idempotency", func(t *testing.T) {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		createKey := "create-approved-" + suffix
		intent, response := createIntent(t, baseURL, apiKey, createKey, "order-approved-"+suffix, "USD", "pm_sim_approved")
		if response.StatusCode != http.StatusCreated || intent.Status != "requires_confirmation" {
			t.Fatalf("create status=%d intent=%+v", response.StatusCode, intent)
		}
		response.Body.Close()

		replayed, replayResponse := createIntent(t, baseURL, apiKey, createKey, "order-approved-"+suffix, "USD", "pm_sim_approved")
		replayResponse.Body.Close()
		if replayResponse.Header.Get("Idempotent-Replayed") != "true" || replayed.ID != intent.ID {
			t.Fatalf("create replay did not return the same resource: first=%s replay=%s header=%q", intent.ID, replayed.ID, replayResponse.Header.Get("Idempotent-Replayed"))
		}
		_, conflictResponse := createIntent(t, baseURL, apiKey, createKey, "different-order-"+suffix, "USD", "pm_sim_approved")
		conflictResponse.Body.Close()
		if conflictResponse.StatusCode != http.StatusConflict {
			t.Fatalf("same idempotency key with a different payload returned %d", conflictResponse.StatusCode)
		}

		confirmed, confirmResponse := confirmIntent(t, baseURL, apiKey, intent.ID, "confirm-approved-"+suffix)
		confirmResponse.Body.Close()
		if confirmResponse.StatusCode != http.StatusOK || confirmed.Status != "succeeded" || confirmed.IntegrityStatus != "healthy" {
			t.Fatalf("confirm status=%d intent=%+v", confirmResponse.StatusCode, confirmed)
		}

		replayedConfirmation, replayedConfirmResponse := confirmIntent(t, baseURL, apiKey, intent.ID, "confirm-approved-"+suffix)
		defer replayedConfirmResponse.Body.Close()
		if replayedConfirmResponse.Header.Get("Idempotent-Replayed") != "true" || replayedConfirmation.ID != intent.ID || replayedConfirmation.Status != "succeeded" {
			t.Fatalf("confirm replay was not stable: intent=%+v header=%q", replayedConfirmation, replayedConfirmResponse.Header.Get("Idempotent-Replayed"))
		}
	})

	t.Run("decline is conclusive and does not fallback", func(t *testing.T) {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		intent, response := createIntent(t, baseURL, apiKey, "create-declined-"+suffix, "order-declined-"+suffix, "USD", "pm_sim_declined")
		response.Body.Close()
		confirmed, confirmResponse := confirmIntent(t, baseURL, apiKey, intent.ID, "confirm-declined-"+suffix)
		defer confirmResponse.Body.Close()
		if confirmResponse.StatusCode != http.StatusOK || confirmed.Status != "failed" {
			t.Fatalf("decline status=%d intent=%+v", confirmResponse.StatusCode, confirmed)
		}
	})

	t.Run("lost response becomes unknown and converges without a new attempt", func(t *testing.T) {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		intent, response := createIntent(t, baseURL, apiKey, "create-timeout-"+suffix, "order-timeout-"+suffix, "USD", "pm_sim_timeout_then_succeeded")
		response.Body.Close()
		processing, confirmResponse := confirmIntent(t, baseURL, apiKey, intent.ID, "confirm-timeout-"+suffix)
		confirmResponse.Body.Close()
		if confirmResponse.StatusCode != http.StatusAccepted || processing.Status != "processing" {
			t.Fatalf("timeout must first be processing: status=%d intent=%+v", confirmResponse.StatusCode, processing)
		}
		final := pollIntent(t, baseURL, apiKey, intent.ID, 15*time.Second)
		if final.Status != "succeeded" || final.IntegrityStatus != "healthy" {
			t.Fatalf("unknown payment did not converge safely: %+v", final)
		}
		assertSingleFundsPath(t, intent.ID)
	})
}

func assertSingleFundsPath(t *testing.T, paymentIntentID string) {
	t.Helper()
	databaseURL := env("E2E_DATABASE_URL", "postgres://payments:payments-local-only@127.0.0.1:5432/payments?sslmode=disable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open E2E database: %v", err)
	}
	defer pool.Close()

	var attempts, paymentOperations, gatewayOperations int
	err = pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM payment_attempts WHERE payment_intent_id=$1),
			(SELECT count(*) FROM provider_operations WHERE payment_intent_id=$1),
			(SELECT count(*)
			 FROM provider_gateway.operations gateway_operation
			 JOIN provider_operations payment_operation
			   ON payment_operation.id=gateway_operation.operation_id
			 WHERE payment_operation.payment_intent_id=$1)`, paymentIntentID,
	).Scan(&attempts, &paymentOperations, &gatewayOperations)
	if err != nil {
		t.Fatalf("count durable funds paths: %v", err)
	}
	if attempts != 1 || paymentOperations != 1 || gatewayOperations != 1 {
		t.Fatalf("UNKNOWN created another funds path: attempts=%d payment_operations=%d gateway_operations=%d", attempts, paymentOperations, gatewayOperations)
	}
	var paymentConnectionID, paymentRevision, paymentNamespace string
	var gatewayConnectionID, gatewayRevision, gatewayNamespace string
	if err := pool.QueryRow(ctx, `
		SELECT payment_operation.provider_connection_id,
		       payment_operation.provider_connection_revision,
		       payment_operation.provider_namespace_id,
		       gateway_operation.provider_connection_id,
		       gateway_operation.provider_connection_revision,
		       gateway_operation.provider_namespace_id
		FROM provider_operations payment_operation
		JOIN provider_gateway.operations gateway_operation
		  ON gateway_operation.operation_id=payment_operation.id
		WHERE payment_operation.payment_intent_id=$1`, paymentIntentID,
	).Scan(&paymentConnectionID, &paymentRevision, &paymentNamespace, &gatewayConnectionID, &gatewayRevision, &gatewayNamespace); err != nil {
		t.Fatalf("read pinned provider connection: %v", err)
	}
	if paymentConnectionID != "simulator_a_us_primary" || paymentRevision != "simulator_a_us_primary_v1" || paymentNamespace != "simulator_a" ||
		gatewayConnectionID != paymentConnectionID || gatewayRevision != paymentRevision || gatewayNamespace != paymentNamespace {
		t.Fatalf("UNKNOWN recovery drifted execution identity: payment=(%q,%q,%q) gateway=(%q,%q,%q)",
			paymentConnectionID, paymentRevision, paymentNamespace, gatewayConnectionID, gatewayRevision, gatewayNamespace)
	}
}

func createIntent(t *testing.T, baseURL, apiKey, idempotencyKey, merchantReference, currency, token string) (paymentIntent, *http.Response) {
	t.Helper()
	body := map[string]any{
		"merchant_reference":   merchantReference,
		"amount_minor":         5200,
		"currency":             currency,
		"payment_method_token": token,
		"capture_method":       "automatic",
	}
	return requestIntent(t, http.MethodPost, baseURL+"/v1/payment_intents", apiKey, idempotencyKey, body)
}

func confirmIntent(t *testing.T, baseURL, apiKey, id, idempotencyKey string) (paymentIntent, *http.Response) {
	t.Helper()
	return requestIntent(t, http.MethodPost, baseURL+"/v1/payment_intents/"+id+"/confirm", apiKey, idempotencyKey, nil)
}

func getIntent(t *testing.T, baseURL, apiKey, id string) paymentIntent {
	t.Helper()
	intent, response := requestIntent(t, http.MethodGet, baseURL+"/v1/payment_intents/"+id, apiKey, "", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET payment intent returned %d", response.StatusCode)
	}
	return intent
}

func requestIntent(t *testing.T, method, endpoint, apiKey, idempotencyKey string, body any) (paymentIntent, *http.Response) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(payload))
	if readErr != nil {
		t.Fatalf("read payment response (HTTP %d): %v", response.StatusCode, readErr)
	}
	var intent paymentIntent
	if err := json.Unmarshal(payload, &intent); err != nil {
		t.Fatalf("decode payment response (HTTP %d): %v %s", response.StatusCode, err, payload)
	}
	return intent, response
}

func pollIntent(t *testing.T, baseURL, apiKey, id string, timeout time.Duration) paymentIntent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		intent := getIntent(t, baseURL, apiKey, id)
		if intent.Status != "processing" {
			return intent
		}
		if time.Now().After(deadline) {
			t.Fatalf("payment %s remained processing after %s", id, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitReady(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := (&http.Client{Timeout: time.Second}).Get(baseURL + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("payment service did not become ready")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
