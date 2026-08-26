package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/app"
	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/YouEclipse/payments/services/payment/internal/postgres"
)

const maxRequestBody = 64 << 10

type Handler struct {
	service paymentService
	store   *postgres.Store
	auth    *Authenticator
	logger  *slog.Logger
	mux     *http.ServeMux
}

type paymentService interface {
	CreateIntent(context.Context, string, string, app.CreateIntentInput) (app.Result, error)
	ConfirmIntent(context.Context, string, string, string) (app.Result, error)
	GetIntent(context.Context, string, string) (app.Result, error)
	ReceiveProviderOutcome(context.Context, domain.ProviderOutcome, []byte) (app.Result, bool, error)
}

func NewHandler(service paymentService, store *postgres.Store, auth *Authenticator, logger *slog.Logger) http.Handler {
	handler := &Handler{service: service, store: store, auth: auth, logger: logger, mux: http.NewServeMux()}
	handler.mux.HandleFunc("GET /livez", handler.livez)
	handler.mux.HandleFunc("GET /readyz", handler.readyz)
	handler.mux.HandleFunc("GET /metrics", handler.metrics)
	handler.mux.HandleFunc("POST /v1/payment_intents", handler.createIntent)
	handler.mux.HandleFunc("POST /v1/payment_intents/{intent_id}/confirm", handler.confirmIntent)
	handler.mux.HandleFunc("GET /v1/payment_intents/{intent_id}", handler.getIntent)
	handler.mux.HandleFunc("POST /internal/v1/provider-outcomes", handler.providerOutcome)
	return handler.requestContext(handler.mux)
}

func (h *Handler) createIntent(response http.ResponseWriter, request *http.Request) {
	merchantID, ok := h.requireMerchant(response, request)
	if !ok {
		return
	}
	if !requireJSON(response, request) {
		return
	}
	var input app.CreateIntentInput
	if err := decodeJSON(response, request, &input); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	result, err := h.service.CreateIntent(request.Context(), merchantID, request.Header.Get("Idempotency-Key"), input)
	if err != nil {
		h.handleError(response, request, err)
		return
	}
	h.writeResult(response, result)
}

func (h *Handler) confirmIntent(response http.ResponseWriter, request *http.Request) {
	merchantID, ok := h.requireMerchant(response, request)
	if !ok {
		return
	}
	if !requireNoBody(response, request) {
		return
	}
	result, err := h.service.ConfirmIntent(request.Context(), merchantID, request.PathValue("intent_id"), request.Header.Get("Idempotency-Key"))
	if err != nil {
		h.handleError(response, request, err)
		return
	}
	h.writeResult(response, result)
}

func (h *Handler) getIntent(response http.ResponseWriter, request *http.Request) {
	merchantID, ok := h.requireMerchant(response, request)
	if !ok {
		return
	}
	result, err := h.service.GetIntent(request.Context(), merchantID, request.PathValue("intent_id"))
	if err != nil {
		h.handleError(response, request, err)
		return
	}
	h.writeResult(response, result)
}

type providerOutcomeRequest struct {
	EventID                    string    `json:"event_id"`
	OperationID                string    `json:"operation_id"`
	AttemptID                  string    `json:"attempt_id,omitempty"`
	ProviderConnectionID       string    `json:"provider_connection_id"`
	ProviderConnectionRevision string    `json:"provider_connection_revision"`
	ProviderNamespaceID        string    `json:"provider_namespace_id"`
	Provider                   string    `json:"provider"`
	ProviderIdempotencyKey     string    `json:"provider_idempotency_key"`
	Status                     string    `json:"status"`
	SideEffectCertainty        string    `json:"side_effect_certainty"`
	ProviderReference          string    `json:"provider_reference,omitempty"`
	FailureCode                string    `json:"failure_code,omitempty"`
	OccurredAt                 time.Time `json:"occurred_at,omitempty"`
}

func (h *Handler) providerOutcome(response http.ResponseWriter, request *http.Request) {
	if err := h.auth.Internal(request); err != nil {
		writeError(response, http.StatusUnauthorized, "unauthorized", "valid internal authentication is required")
		return
	}
	if !requireJSON(response, request) {
		return
	}
	var input providerOutcomeRequest
	if err := decodeJSON(response, request, &input); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	raw, _ := json.Marshal(input)
	outcome, err := clients.NormalizeGatewayResponse(clients.GatewayOperationResponse{
		OperationID:                input.OperationID,
		AttemptID:                  input.AttemptID,
		ProviderConnectionID:       input.ProviderConnectionID,
		ProviderConnectionRevision: input.ProviderConnectionRevision,
		ProviderNamespaceID:        input.ProviderNamespaceID,
		Provider:                   input.Provider,
		ProviderIdempotencyKey:     input.ProviderIdempotencyKey,
		Status:                     input.Status,
		SideEffectCertainty:        input.SideEffectCertainty,
		ProviderReference:          input.ProviderReference,
		FailureCode:                input.FailureCode,
	})
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_provider_outcome", err.Error())
		return
	}
	outcome.EventID = input.EventID
	outcome.OccurredAt = input.OccurredAt
	result, duplicate, err := h.service.ReceiveProviderOutcome(request.Context(), outcome, raw)
	if err != nil && !errors.Is(err, domain.ErrConflictingOutcome) {
		h.handleError(response, request, err)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(providerOutcomeAcknowledgement(result, duplicate))
}

func providerOutcomeAcknowledgement(result app.Result, duplicate bool) map[string]any {
	payload := map[string]any{
		"accepted":    true,
		"duplicate":   duplicate,
		"quarantined": result.Quarantined,
	}
	// A poison event can name an operation that never existed. It is still
	// durably quarantined and acknowledged so it cannot block the gateway
	// outbox, but no fake Payment Intent identity is synthesized for the ACK.
	if result.Intent.ID != "" {
		payload["integrity_status"] = result.Intent.IntegrityStatus
		payload["payment_intent_id"] = result.Intent.ID
	}
	return payload
}

func (h *Handler) livez(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) readyz(response http.ResponseWriter, request *http.Request) {
	context, cancel := contextWithTimeout(request, time.Second)
	defer cancel()
	if err := h.store.Ping(context); err != nil {
		writeError(response, http.StatusServiceUnavailable, "not_ready", "database is unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) metrics(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = io.WriteString(response, "# HELP payment_service_up Whether the payment service process is up.\n# TYPE payment_service_up gauge\npayment_service_up 1\n")
}

func (h *Handler) requireMerchant(response http.ResponseWriter, request *http.Request) (string, bool) {
	merchantID, err := h.auth.Merchant(request)
	if err != nil {
		response.Header().Set("WWW-Authenticate", `Bearer realm="payments"`)
		writeError(response, http.StatusUnauthorized, "unauthorized", "a valid merchant API key is required")
		return "", false
	}
	return merchantID, true
}

func (h *Handler) writeResult(response http.ResponseWriter, result app.Result) {
	response.Header().Set("Content-Type", "application/json")
	if result.Replay {
		response.Header().Set("Idempotent-Replayed", "true")
	}
	response.WriteHeader(result.Status)
	_, _ = response.Write(result.Body)
}

func (h *Handler) handleError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, idempotency.ErrKeyRequired), errors.Is(err, idempotency.ErrKeyInvalid), errors.Is(err, idempotency.ErrKeyTooLong), errors.Is(err, app.ErrInvalidRequest):
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, postgres.ErrNotFound):
		writeError(response, http.StatusNotFound, "payment_intent_not_found", "payment intent was not found")
	case errors.Is(err, postgres.ErrIdempotencyConflict):
		writeError(response, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used for a different request")
	case errors.Is(err, postgres.ErrMerchantReferenceConflict):
		writeError(response, http.StatusConflict, "merchant_reference_exists", "merchant_reference already identifies another payment intent")
	case errors.Is(err, postgres.ErrStateConflict), errors.Is(err, domain.ErrInvalidTransition):
		writeError(response, http.StatusConflict, "payment_state_conflict", "operation is not allowed from the current payment state")
	case errors.Is(err, clients.ErrNoEligibleProvider):
		writeError(response, http.StatusUnprocessableEntity, "no_eligible_provider", "no provider is eligible for this payment")
	case errors.Is(err, clients.ErrUpstream):
		writeError(response, http.StatusServiceUnavailable, "dependency_unavailable", "an internal dependency is unavailable")
	default:
		h.logger.Error("payment API request failed", slog.String("method", request.Method), slog.String("path", request.URL.Path), slog.String("error", err.Error()))
		writeError(response, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

func (h *Handler) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(response, request)
	})
}

func requireJSON(response http.ResponseWriter, request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(response, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	return true
}

func requireNoBody(response http.ResponseWriter, request *http.Request) bool {
	if request.Body == nil || request.Body == http.NoBody {
		return true
	}
	var firstByte [1]byte
	read, err := request.Body.Read(firstByte[:])
	if read > 0 {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body must be empty")
		return false
	}
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body could not be read")
		return false
	}
	return true
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}

func writeError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func contextWithTimeout(request *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), timeout)
}
