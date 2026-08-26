package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/YouEclipse/payments/services/router/internal/decision"
	"github.com/YouEclipse/payments/services/router/internal/routing"
	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/strictjson"
)

const contextMaxAge = 5 * time.Minute

func (h *Handler) routingDecisionsV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, apiError{Code: "method_not_allowed", Message: "only POST is supported"})
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, http.StatusUnsupportedMediaType, apiError{Code: "unsupported_media_type", Message: err.Error()})
		return
	}
	if h.v2 == nil {
		writeError(w, http.StatusServiceUnavailable, apiError{Code: "routing_v2_unavailable", Message: "Router v2 snapshots have not been configured"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{Code: "request_too_large", Message: "request body exceeds its size limit"})
		} else {
			writeError(w, http.StatusBadRequest, apiError{Code: "invalid_json", Message: "request body could not be read"})
		}
		return
	}
	var context routingcontext.Context
	if err := strictjson.Decode(body, &context); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Code: "invalid_json", Message: err.Error()})
		return
	}
	now := h.now().UTC()
	if err := context.ValidateOnline(now, contextMaxAge); err != nil {
		writeContextValidationError(w, err)
		return
	}
	if !h.v2.ReadyAt(now) {
		writeSnapshotUnavailable(w)
		return
	}
	result, err := h.v2.Decide(context)
	if errors.Is(err, routing.ErrNoEligibleProvider) {
		writeJSON(w, http.StatusUnprocessableEntity, noEligibleProviderV2Response{
			Error:    apiError{Code: "no_eligible_provider", Message: "no connection satisfies the active capability and rule constraints"},
			Decision: result,
		})
		return
	}
	if err != nil {
		h.logger.Error("v2 routing decision failed")
		writeError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "routing decision failed"})
		return
	}
	// Re-read time after evaluation: a GC pause or scheduler delay must not
	// extend either Payment observations or Router-owned capability authority.
	finishedAt := h.now().UTC()
	if err := context.ValidateOnline(finishedAt, contextMaxAge); err != nil {
		writeContextValidationError(w, err)
		return
	}
	if !h.v2.ExecutableAt(result, finishedAt) {
		writeSnapshotUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeContextValidationError(w http.ResponseWriter, err error) {
	response := apiError{Code: "invalid_request", Message: "invalid versioned routing context"}
	var validation *routingcontext.ValidationError
	if errors.As(err, &validation) {
		response.Field, response.Message = validation.Field, validation.Message
	}
	writeError(w, http.StatusBadRequest, response)
}

type noEligibleProviderV2Response struct {
	Error apiError `json:"error"`
	decision.Decision
}

func writeSnapshotUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, apiError{Code: "routing_snapshot_unavailable", Message: "Router-owned snapshots are outside their effective freshness interval"})
}
