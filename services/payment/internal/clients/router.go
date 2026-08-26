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

	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
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
	// Context selects the explicit v2 contract. Nil is the retained legacy v1
	// reader-first path, never an automatic fallback after a v2 error.
	Context       *routingcontext.Context `json:"-"`
	PaymentID     string                  `json:"payment_id"`
	AttemptID     string                  `json:"attempt_id"`
	MerchantID    string                  `json:"merchant_id"`
	AmountMinor   int64                   `json:"amount_minor"`
	Currency      string                  `json:"currency"`
	PaymentMethod string                  `json:"payment_method"`
	Operation     string                  `json:"operation"`
}

type RoutingDecision struct {
	DecisionID               string                `json:"decision_id"`
	SelectedCandidate        RouteCandidate        `json:"selected_candidate"`
	CandidateOrder           []string              `json:"candidate_order"`
	Exclusions               []CandidateExclusion  `json:"exclusions"`
	PolicyVersion            string                `json:"policy_version"`
	ContextSchemaVersion     string                `json:"context_schema_version,omitempty"`
	ContextDigest            string                `json:"context_digest,omitempty"`
	RuleSetVersion           string                `json:"rule_set_version,omitempty"`
	RuleSetDigest            string                `json:"rule_set_digest,omitempty"`
	CapabilityCatalogVersion string                `json:"capability_catalog_version,omitempty"`
	CapabilityCatalogDigest  string                `json:"capability_catalog_digest,omitempty"`
	RuleHits                 []RuleHit             `json:"rule_hits,omitempty"`
	Eligibility              []EligibilityEvidence `json:"eligibility,omitempty"`
	ReasonCodes              []string              `json:"reason_codes,omitempty"`
}

type RuleHit struct {
	RuleID       string   `json:"rule_id"`
	Truth        string   `json:"truth"`
	Action       string   `json:"action"`
	ReasonCode   string   `json:"reason_code"`
	CandidateIDs []string `json:"candidate_ids"`
}

type EligibilityEvidence struct {
	CandidateID string            `json:"candidate_id"`
	Status      string            `json:"status"`
	Reasons     []ExclusionReason `json:"reasons"`
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
	var requestBody any = routingRequest
	path := "/internal/v1/routing-decisions"
	expectedDigest := ""
	if routingRequest.Context != nil {
		var err error
		expectedDigest, err = routingRequest.Context.Digest()
		if err != nil {
			return RoutingDecision{}, fmt.Errorf("%w: invalid routing context", ErrUpstream)
		}
		requestBody = routingRequest.Context
		path = "/internal/v2/routing-decisions"
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("marshal routing request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
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
	payload, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: read router response: %v", ErrUpstream, err)
	}
	if len(payload) > 1<<20 {
		return RoutingDecision{}, fmt.Errorf("%w: router response exceeds size limit", ErrUpstream)
	}
	var decision RoutingDecision
	if err := json.Unmarshal(payload, &decision); err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: decode router response: %v", ErrUpstream, err)
	}
	if expectedDigest != "" && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusUnprocessableEntity) {
		if decision.ContextSchemaVersion != routingcontext.SchemaVersion || decision.ContextDigest != expectedDigest ||
			decision.DecisionID == "" || decision.RuleSetVersion == "" || decision.PolicyVersion != decision.RuleSetVersion ||
			!validArtifactDigest(decision.RuleSetDigest) || decision.CapabilityCatalogVersion == "" || !validArtifactDigest(decision.CapabilityCatalogDigest) {
			return RoutingDecision{}, fmt.Errorf("%w: router context or artifact evidence mismatch", ErrUpstream)
		}
	}
	if response.StatusCode == http.StatusUnprocessableEntity {
		if expectedDigest != "" && (decision.SelectedCandidate != (RouteCandidate{}) || len(decision.CandidateOrder) != 0) {
			return RoutingDecision{}, fmt.Errorf("%w: rejected routing decision has a candidate", ErrUpstream)
		}
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
	seen := make(map[string]bool, len(decision.CandidateOrder))
	eligible := make(map[string]bool, len(decision.Eligibility))
	if expectedDigest != "" {
		for _, evidence := range decision.Eligibility {
			if _, duplicate := eligible[evidence.CandidateID]; duplicate || !validPlatformIdentifier(evidence.CandidateID) ||
				(evidence.Status != "eligible" && evidence.Status != "ineligible" && evidence.Status != "unknown") {
				return RoutingDecision{}, fmt.Errorf("%w: router eligibility evidence is invalid", ErrUpstream)
			}
			eligible[evidence.CandidateID] = evidence.Status == "eligible"
		}
	}
	for _, candidate := range decision.CandidateOrder {
		if !validPlatformIdentifier(candidate) || seen[candidate] || (expectedDigest != "" && !eligible[candidate]) {
			return RoutingDecision{}, fmt.Errorf("%w: router candidate order is invalid", ErrUpstream)
		}
		seen[candidate] = true
	}
	return decision, nil
}

func validArtifactDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	for _, character := range value[7:] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
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
