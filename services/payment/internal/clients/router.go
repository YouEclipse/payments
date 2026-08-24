package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var (
	ErrNoEligibleProvider       = errors.New("no eligible provider")
	ErrUpstream                 = errors.New("internal upstream request failed")
	ErrGatewayOperationNotFound = errors.New("gateway operation not found")
)

type RouterClient struct {
	baseURL       string
	internalToken string
	client        *http.Client
}

func NewRouterClient(baseURL, internalToken string, client *http.Client) (*RouterClient, error) {
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("router base URL: %w", err)
	}
	if internalToken == "" {
		return nil, errors.New("router internal token is required")
	}
	return &RouterClient{baseURL: normalized, internalToken: internalToken, client: client}, nil
}

type RoutingRequest struct {
	PaymentID     string `json:"payment_id"`
	AttemptID     string `json:"attempt_id"`
	MerchantID    string `json:"merchant_id"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	PaymentMethod string `json:"payment_method"`
	Operation     string `json:"operation"`
}

type RoutingDecision struct {
	DecisionID       string              `json:"decision_id"`
	SelectedProvider string              `json:"selected_provider"`
	CandidateOrder   []string            `json:"candidate_order"`
	Exclusions       []ProviderExclusion `json:"exclusions"`
	PolicyVersion    string              `json:"policy_version"`
}

type ProviderExclusion struct {
	Provider string            `json:"provider"`
	Reasons  []ExclusionReason `json:"reasons"`
}

type ExclusionReason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (c *RouterClient) Decide(ctx context.Context, routingRequest RoutingRequest) (RoutingDecision, error) {
	body, err := json.Marshal(routingRequest)
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("marshal routing request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/v1/routing-decisions", bytes.NewReader(body))
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("build routing request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Internal-Token", c.internalToken)

	response, err := c.client.Do(request)
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: router transport: %v", ErrUpstream, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: read router response: %v", ErrUpstream, err)
	}
	var decision RoutingDecision
	if err := json.Unmarshal(payload, &decision); err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: decode router response: %v", ErrUpstream, err)
	}
	if response.StatusCode == http.StatusUnprocessableEntity {
		return decision, ErrNoEligibleProvider
	}
	if response.StatusCode != http.StatusOK {
		return RoutingDecision{}, fmt.Errorf("%w: router returned HTTP %d", ErrUpstream, response.StatusCode)
	}
	if decision.DecisionID == "" || decision.SelectedProvider == "" {
		return RoutingDecision{}, fmt.Errorf("%w: router returned an incomplete decision", ErrUpstream)
	}
	return decision, nil
}

func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("URL scheme must be http or https")
	}
	if parsed.Host == "" || parsed.User != nil {
		return "", errors.New("URL must have a host and must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("URL must not contain a query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}
