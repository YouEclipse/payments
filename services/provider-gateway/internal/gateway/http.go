package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

type HTTPHandler struct {
	service       *Service
	store         Store
	internalToken string
}

func NewHTTPHandler(service *Service, store Store, internalToken string) http.Handler {
	h := &HTTPHandler{service: service, store: store, internalToken: internalToken}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "# HELP provider_gateway_up Whether the provider gateway process is up.\n# TYPE provider_gateway_up gauge\nprovider_gateway_up 1\n")
	})
	mux.Handle("POST /internal/v1/provider-operations", h.requireInternal(http.HandlerFunc(h.execute)))
	mux.Handle("GET /internal/v1/provider-operations/{operation_id}", h.requireInternal(http.HandlerFunc(h.get)))
	mux.HandleFunc("POST /webhooks/{provider}", h.webhook)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (h *HTTPHandler) ready(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "required storage is unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) execute(w http.ResponseWriter, r *http.Request) {
	var input OperationRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	op, err := h.service.Execute(r.Context(), input)
	if err != nil {
		switch {
		case errors.Is(err, ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "operation_id_reused", err.Error())
		case errors.Is(err, ErrInvalidOperation):
			writeError(w, http.StatusUnprocessableEntity, "operation_rejected", err.Error())
		default:
			writeError(w, http.StatusServiceUnavailable, "operation_persistence_unavailable", "the operation could not be durably processed")
		}
		return
	}
	status := http.StatusOK
	if op.Status == StatusUnknown {
		status = http.StatusAccepted
	}
	writeJSON(w, status, op)
}

func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request) {
	reconcile := r.URL.Query().Get("reconcile") == "true"
	op, err := h.service.Get(r.Context(), r.PathValue("operation_id"), reconcile)
	if errors.Is(err, ErrResubmitRequired) {
		// Payment owns the original tokenized request payload. A 404 on the
		// reconcile form tells it to resubmit that exact operation; Execute then
		// verifies the durable request hash before any provider call.
		writeError(w, http.StatusNotFound, "operation_resubmit_required", err.Error())
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "operation_not_found", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "operation lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (h *HTTPHandler) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook", "webhook body is too large or unreadable")
		return
	}
	_, err = h.service.ApplyWebhook(r.Context(), r.PathValue("provider"), body, r.Header.Get("X-Provider-Signature"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidWebhook):
			writeError(w, http.StatusBadRequest, "invalid_webhook", err.Error())
		case errors.Is(err, ErrWebhookEventConflict):
			writeError(w, http.StatusConflict, "webhook_event_conflict", "event_id was already used for different event data")
		case errors.Is(err, ErrNotFound):
			// The callback may race with durable operation creation. A retryable
			// response prevents a provider from treating that race as a permanent
			// malformed request.
			writeError(w, http.StatusServiceUnavailable, "webhook_operation_unavailable", "the referenced operation is not available yet")
		default:
			writeError(w, http.StatusServiceUnavailable, "webhook_persistence_unavailable", "the webhook could not be durably stored")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) requireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Internal-Token")
		if h.internalToken == "" || len(provided) != len(h.internalToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(h.internalToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "internal authentication failed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
