package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
)

type GatewayClient struct {
	baseURL       string
	internalToken string
	client        *http.Client
}

func NewGatewayClient(baseURL, internalToken string, client *http.Client) (*GatewayClient, error) {
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway base URL: %w", err)
	}
	if internalToken == "" {
		return nil, fmt.Errorf("gateway internal token is required")
	}
	return &GatewayClient{baseURL: normalized, internalToken: internalToken, client: client}, nil
}

type GatewayOperationRequest struct {
	OperationID        string `json:"operation_id"`
	AttemptID          string `json:"attempt_id"`
	Provider           string `json:"provider"`
	AmountMinor        int64  `json:"amount_minor"`
	Currency           string `json:"currency"`
	PaymentMethodToken string `json:"payment_method_token"`
	CaptureMethod      string `json:"capture_method"`
	Scenario           string `json:"scenario,omitempty"`
}

type GatewayOperationResponse struct {
	OperationID            string `json:"operation_id"`
	AttemptID              string `json:"attempt_id"`
	Provider               string `json:"provider"`
	ProviderIdempotencyKey string `json:"provider_idempotency_key"`
	Status                 string `json:"status"`
	SideEffectCertainty    string `json:"side_effect_certainty"`
	ProviderReference      string `json:"provider_reference,omitempty"`
	FailureCode            string `json:"failure_code,omitempty"`
}

func (c *GatewayClient) Execute(ctx context.Context, command GatewayOperationRequest) (domain.ProviderOutcome, error) {
	body, err := json.Marshal(command)
	if err != nil {
		return domain.ProviderOutcome{}, fmt.Errorf("marshal gateway operation: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/v1/provider-operations", bytes.NewReader(body))
	if err != nil {
		return domain.ProviderOutcome{}, fmt.Errorf("build gateway operation: %w", err)
	}
	return c.do(request, command.OperationID)
}

func (c *GatewayClient) Query(ctx context.Context, operationID string) (domain.ProviderOutcome, error) {
	endpoint := c.baseURL + "/internal/v1/provider-operations/" + url.PathEscape(operationID) + "?reconcile=true"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return domain.ProviderOutcome{}, fmt.Errorf("build gateway query: %w", err)
	}
	return c.do(request, operationID)
}

func (c *GatewayClient) do(request *http.Request, expectedOperationID string) (domain.ProviderOutcome, error) {
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Internal-Token", c.internalToken)
	if request.Method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return domain.ProviderOutcome{}, fmt.Errorf("%w: gateway transport: %v", ErrUpstream, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return domain.ProviderOutcome{}, fmt.Errorf("%w: read gateway response: %v", ErrUpstream, err)
	}
	if response.StatusCode == http.StatusNotFound {
		return domain.ProviderOutcome{}, ErrGatewayOperationNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return domain.ProviderOutcome{}, fmt.Errorf("%w: gateway returned HTTP %d", ErrUpstream, response.StatusCode)
	}
	var gatewayResponse GatewayOperationResponse
	if err := json.Unmarshal(payload, &gatewayResponse); err != nil {
		return domain.ProviderOutcome{}, fmt.Errorf("%w: decode gateway response: %v", ErrUpstream, err)
	}
	if gatewayResponse.OperationID != expectedOperationID {
		return domain.ProviderOutcome{}, fmt.Errorf("%w: gateway operation ID mismatch", ErrUpstream)
	}
	return NormalizeGatewayResponse(gatewayResponse)
}

func NormalizeGatewayResponse(response GatewayOperationResponse) (domain.ProviderOutcome, error) {
	certainty := domain.SideEffectCertainty(response.SideEffectCertainty)
	switch certainty {
	case domain.SideEffectNone, domain.SideEffectPossible, domain.SideEffectConfirmed:
	default:
		return domain.ProviderOutcome{}, fmt.Errorf("%w: invalid side_effect_certainty %q", ErrUpstream, response.SideEffectCertainty)
	}
	outcome := domain.ProviderOutcome{
		Provider:                   response.Provider,
		OperationID:                response.OperationID,
		ProviderIdempotencyKey:     response.ProviderIdempotencyKey,
		SideEffectCertainty:        certainty,
		ProviderPaymentReference:   response.ProviderReference,
		ProviderOperationReference: response.ProviderReference,
		NormalizedCode:             response.FailureCode,
		ProviderCode:               response.FailureCode,
	}
	switch response.Status {
	case "succeeded":
		outcome.Status = domain.OutcomeSucceeded
		outcome.SideEffectCertainty = domain.SideEffectConfirmed
		outcome.RetryMode = domain.RetryNone
	case "failed":
		if certainty != domain.SideEffectNone {
			outcome.Status = domain.OutcomeUnknown
			outcome.SideEffectCertainty = domain.SideEffectPossible
			outcome.RetryMode = domain.RetryQueryOrWait
			break
		}
		if isDeclineCode(response.FailureCode) {
			outcome.Status = domain.OutcomeDeclined
		} else {
			outcome.Status = domain.OutcomeFailed
		}
		outcome.RetryMode = domain.RetryNone
	case "created", "sending":
		outcome.Status = domain.OutcomePending
		outcome.SideEffectCertainty = domain.SideEffectPossible
		outcome.RetryMode = domain.RetryQueryOrWait
	case "unknown", "review_required":
		outcome.Status = domain.OutcomeUnknown
		outcome.SideEffectCertainty = domain.SideEffectPossible
		outcome.RetryMode = domain.RetryQueryOrWait
		outcome.RequiresReview = response.Status == "review_required"
	default:
		return domain.ProviderOutcome{}, fmt.Errorf("%w: invalid gateway status %q", ErrUpstream, response.Status)
	}
	return outcome, nil
}

func isDeclineCode(code string) bool {
	switch strings.ToLower(code) {
	case "declined", "card_declined", "do_not_honor", "insufficient_funds":
		return true
	default:
		return false
	}
}
