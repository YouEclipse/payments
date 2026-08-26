package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/routing"
)

const maxRequestBodyBytes = 1 << 20

type decisionEngine interface {
	Decide(routing.Request) (routing.Decision, error)
}

// Handler exposes the router's internal HTTP API.
type Handler struct {
	engine decisionEngine
	v2     *decision.Engine
	now    func() time.Time
	logger *slog.Logger
}

func NewHandler(engine decisionEngine, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{engine: engine, logger: logger, now: time.Now}
}

// WithV2 installs an independently configured reader-first v2 engine. Nil
// deliberately leaves v2 unavailable; it never changes the legacy v1 engine.
func (h *Handler) WithV2(engine *decision.Engine) *Handler { h.v2 = engine; return h }

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/livez", h.health)
	mux.HandleFunc("/readyz", h.health)
	mux.HandleFunc("/metrics", h.metrics)
	mux.HandleFunc("/internal/v1/routing-decisions", h.routingDecisions)
	mux.HandleFunc("/internal/v2/routing-decisions", h.routingDecisionsV2)
}

func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, apiError{Code: "method_not_allowed", Message: "only GET is supported"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = io.WriteString(w, "# HELP payment_router_up Whether the payment router process is up.\n# TYPE payment_router_up gauge\npayment_router_up 1\n")
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, apiError{
			Code:    "method_not_allowed",
			Message: "only GET is supported",
		})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (h *Handler) routingDecisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, apiError{
			Code:    "method_not_allowed",
			Message: "only POST is supported",
		})
		return
	}

	if err := requireJSONContentType(r); err != nil {
		writeError(w, http.StatusUnsupportedMediaType, apiError{
			Code:    "unsupported_media_type",
			Message: err.Error(),
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer r.Body.Close()

	var payload routingDecisionRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{
				Code:    "request_too_large",
				Message: fmt.Sprintf("request body must not exceed %d bytes", maxRequestBodyBytes),
			})
			return
		}
		writeError(w, http.StatusBadRequest, apiError{
			Code:    "invalid_json",
			Message: "request body must be one valid JSON object: " + err.Error(),
		})
		return
	}
	if err := ensureEOF(decoder); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Code:    "invalid_json",
			Message: err.Error(),
		})
		return
	}

	req := payload.domainRequest()
	decision, err := h.engine.Decide(req)
	if err == nil {
		writeJSON(w, http.StatusOK, decision)
		return
	}

	if errors.Is(err, routing.ErrNoEligibleProvider) {
		writeJSON(w, http.StatusUnprocessableEntity, noEligibleProviderResponse{
			Error: apiError{
				Code:    "no_eligible_provider",
				Message: "no provider satisfies the active eligibility constraints and policy",
			},
			DecisionID:     decision.ID,
			CandidateOrder: decision.CandidateOrder,
			Exclusions:     decision.Exclusions,
			PolicyVersion:  decision.PolicyVersion,
		})
		return
	}

	var validationErr *routing.ValidationError
	if errors.As(err, &validationErr) {
		writeValidationError(w, validationErr)
		return
	}

	h.logger.Error("routing decision failed", "error", err)
	writeError(w, http.StatusInternalServerError, apiError{
		Code:    "internal_error",
		Message: "routing decision failed",
	})
}

func requireJSONContentType(r *http.Request) error {
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		return errors.New("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("request body must contain exactly one JSON object")
	}
	return errors.New("request body must contain exactly one valid JSON object")
}

func writeValidationError(w http.ResponseWriter, err error) {
	response := apiError{
		Code:    "invalid_request",
		Message: err.Error(),
	}
	var validationErr *routing.ValidationError
	if errors.As(err, &validationErr) {
		response.Field = validationErr.Field
	}
	writeError(w, http.StatusBadRequest, response)
}

func writeError(w http.ResponseWriter, status int, err apiError) {
	writeJSON(w, status, errorEnvelope{Error: err})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Default().Error("encode HTTP response", "error", err)
	}
}

type healthResponse struct {
	Status string `json:"status"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

type noEligibleProviderResponse struct {
	Error          apiError            `json:"error"`
	DecisionID     string              `json:"decision_id"`
	CandidateOrder []string            `json:"candidate_order"`
	Exclusions     []routing.Exclusion `json:"exclusions"`
	PolicyVersion  string              `json:"policy_version"`
}

type routingDecisionRequest struct {
	PaymentID     string `json:"payment_id"`
	AttemptID     string `json:"attempt_id"`
	MerchantID    string `json:"merchant_id"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	PaymentMethod string `json:"payment_method"`
	Operation     string `json:"operation"`
}

func (r routingDecisionRequest) domainRequest() routing.Request {
	return routing.Request{
		PaymentID:     r.PaymentID,
		AttemptID:     r.AttemptID,
		MerchantID:    r.MerchantID,
		AmountMinor:   r.AmountMinor,
		Currency:      r.Currency,
		PaymentMethod: r.PaymentMethod,
		Operation:     r.Operation,
	}
}
