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
	ErrGatewayIdentityMismatch  = fmt.Errorf("%w: gateway execution identity mismatch", ErrUpstream)
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
	DecisionID        string               `json:"decision_id"`
	SelectedCandidate RouteCandidate       `json:"selected_candidate"`
	CandidateOrder    []string             `json:"candidate_order"`
	Exclusions        []CandidateExclusion `json:"exclusions"`
	PolicyVersion     string               `json:"policy_version"`
}

type RouteCandidate struct {
	CandidateID         string `json:"candidate_id"`
	ConnectionRevision  string `json:"connection_revision"`
	ProviderNamespaceID string `json:"provider_namespace_id"`
	Provider            string `json:"provider"`
	MerchantAccount     string `json:"merchant_account"`
	Connector           string `json:"connector"`
	Region              string `json:"region"`
}

type CandidateExclusion struct {
	CandidateID string            `json:"candidate_id"`
	Provider    string            `json:"provider"`
	Reasons     []ExclusionReason `json:"reasons"`
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
	selected := decision.SelectedCandidate
	if decision.DecisionID == "" || !validPlatformIdentifier(selected.CandidateID) || !validPlatformIdentifier(selected.ConnectionRevision) ||
		!validPlatformIdentifier(selected.ProviderNamespaceID) || selected.Provider == "" ||
		selected.MerchantAccount == "" || selected.Connector == "" || selected.Region == "" {
		return RoutingDecision{}, fmt.Errorf("%w: router returned an incomplete decision", ErrUpstream)
	}
	if len(decision.CandidateOrder) == 0 || decision.CandidateOrder[0] != selected.CandidateID {
		return RoutingDecision{}, fmt.Errorf("%w: router selected candidate does not match candidate order", ErrUpstream)
	}
	return decision, nil
}

func validPlatformIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			(index > 0 && (character == '.' || character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
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
