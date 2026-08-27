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
	"time"

	"github.com/YouEclipse/payments/internal/riskdecision"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

var (
	ErrNoEligibleProvider       = errors.New("no eligible provider")
	ErrRiskBlocked              = fmt.Errorf("%w: risk blocked", ErrNoEligibleProvider)
	ErrAuthenticationRequired   = fmt.Errorf("%w: authentication unsupported", ErrNoEligibleProvider)
	ErrRiskReviewRequired       = fmt.Errorf("%w: risk review required", ErrNoEligibleProvider)
	ErrUpstream                 = errors.New("internal upstream request failed")
	ErrGatewayOperationNotFound = errors.New("gateway operation not found")
	ErrGatewayIdentityMismatch  = fmt.Errorf("%w: gateway execution identity mismatch", ErrUpstream)
)

type RouterClient struct {
	baseURL       string
	internalToken string
	client        *http.Client
	now           func() time.Time
}

func NewRouterClient(baseURL, internalToken string, client *http.Client) (*RouterClient, error) {
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("router base URL: %w", err)
	}
	if internalToken == "" {
		return nil, errors.New("router internal token is required")
	}
	return &RouterClient{baseURL: normalized, internalToken: internalToken, client: client, now: time.Now}, nil
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
	Error                    *RouterErrorEvidence `json:"error,omitempty"`
	DecisionID               string               `json:"decision_id"`
	SelectedCandidate        RouteCandidate       `json:"selected_candidate"`
	CandidateOrder           []string             `json:"candidate_order"`
	Exclusions               []CandidateExclusion `json:"exclusions"`
	PolicyVersion            string               `json:"policy_version"`
	ContextSchemaVersion     string               `json:"context_schema_version,omitempty"`
	ContextDigest            string               `json:"context_digest,omitempty"`
	ExecutableUntil          time.Time            `json:"executable_until,omitempty"`
	RuleSetVersion           string               `json:"rule_set_version,omitempty"`
	RuleSetDigest            string               `json:"rule_set_digest,omitempty"`
	CapabilityCatalogVersion string               `json:"capability_catalog_version,omitempty"`
	CapabilityCatalogDigest  string               `json:"capability_catalog_digest,omitempty"`
	// These arrays are part of Router's canonical decision envelope. They must
	// remain present even when empty, otherwise Payment changes the evidence
	// before persisting it and the content-derived decision ID no longer binds
	// the stored snapshot.
	RuleHits    []RuleHit              `json:"rule_hits"`
	Eligibility []EligibilityEvidence  `json:"eligibility"`
	ReasonCodes []string               `json:"reason_codes"`
	Risk        *riskdecision.Evidence `json:"risk,omitempty"`
	// The Router owns feature mathematics. Payment validates this credential-
	// free envelope and its execution binding, then preserves it without loss.
	Optimization json.RawMessage `json:"optimization,omitempty"`
}

// MarshalJSON mirrors Router's wire envelope, including pointer semantics for
// an absent winner and Router's canonical evidence order. Payment persists this
// representation; a rejected decision must never manufacture an empty
// selected_candidate object merely because its local projection uses a value.
func (d RoutingDecision) MarshalJSON() ([]byte, error) {
	var selected *RouteCandidate
	if d.SelectedCandidate != (RouteCandidate{}) {
		copy := d.SelectedCandidate
		selected = &copy
	}
	var executableUntil *time.Time
	if !d.ExecutableUntil.IsZero() {
		copy := d.ExecutableUntil
		executableUntil = &copy
	}
	wire := struct {
		Error                    *RouterErrorEvidence   `json:"error,omitempty"`
		DecisionID               string                 `json:"decision_id"`
		SelectedCandidate        *RouteCandidate        `json:"selected_candidate,omitempty"`
		CandidateOrder           []string               `json:"candidate_order"`
		Exclusions               []CandidateExclusion   `json:"exclusions"`
		PolicyVersion            string                 `json:"policy_version"`
		ContextSchemaVersion     string                 `json:"context_schema_version,omitempty"`
		ContextDigest            string                 `json:"context_digest,omitempty"`
		ExecutableUntil          *time.Time             `json:"executable_until,omitempty"`
		RuleSetVersion           string                 `json:"rule_set_version,omitempty"`
		RuleSetDigest            string                 `json:"rule_set_digest,omitempty"`
		CapabilityCatalogVersion string                 `json:"capability_catalog_version,omitempty"`
		CapabilityCatalogDigest  string                 `json:"capability_catalog_digest,omitempty"`
		RuleHits                 []RuleHit              `json:"rule_hits"`
		Eligibility              []EligibilityEvidence  `json:"eligibility"`
		Risk                     *riskdecision.Evidence `json:"risk,omitempty"`
		Optimization             json.RawMessage        `json:"optimization,omitempty"`
		ReasonCodes              []string               `json:"reason_codes"`
	}{d.Error, d.DecisionID, selected, d.CandidateOrder, d.Exclusions, d.PolicyVersion,
		d.ContextSchemaVersion, d.ContextDigest, executableUntil, d.RuleSetVersion, d.RuleSetDigest,
		d.CapabilityCatalogVersion, d.CapabilityCatalogDigest, d.RuleHits, d.Eligibility,
		d.Risk, d.Optimization, d.ReasonCodes}
	return json.Marshal(wire)
}

type RouterErrorEvidence struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
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
		if routingRequest.Context.SchemaVersion == routingcontext.RiskSchemaVersion {
			path = "/internal/v3/routing-decisions"
		}
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
	if err := validateUniqueJSON(payload); err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: ambiguous router response", ErrUpstream)
	}
	if expectedDigest != "" && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusUnprocessableEntity) {
		if err := validateVersionedDecisionJSON(payload, routingRequest.Context.SchemaVersion == routingcontext.RiskSchemaVersion, response.StatusCode == http.StatusOK); err != nil {
			return RoutingDecision{}, fmt.Errorf("%w: incomplete router response envelope", ErrUpstream)
		}
	}
	var decision RoutingDecision
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return RoutingDecision{}, fmt.Errorf("%w: decode router response: %v", ErrUpstream, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return RoutingDecision{}, fmt.Errorf("%w: router response must contain one object", ErrUpstream)
	}
	if expectedDigest != "" && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusUnprocessableEntity) {
		contextUntil, authorityErr := routingRequest.Context.OnlineAuthorityUntil(routingcontext.OnlineMaxAge)
		receivedAt := c.now().UTC()
		if decision.ContextSchemaVersion != routingRequest.Context.SchemaVersion || decision.ContextDigest != expectedDigest ||
			decision.DecisionID == "" || decision.RuleSetVersion == "" || decision.PolicyVersion != decision.RuleSetVersion ||
			!validArtifactDigest(decision.RuleSetDigest) || decision.CapabilityCatalogVersion == "" || !validArtifactDigest(decision.CapabilityCatalogDigest) ||
			authorityErr != nil || decision.ExecutableUntil.IsZero() || !decision.ExecutableUntil.Equal(decision.ExecutableUntil.Truncate(time.Microsecond)) ||
			!receivedAt.Before(decision.ExecutableUntil) || decision.ExecutableUntil.After(contextUntil) {
			return RoutingDecision{}, fmt.Errorf("%w: router context or artifact evidence mismatch", ErrUpstream)
		}
		if !validVersionedDecisionID(decision) {
			return RoutingDecision{}, fmt.Errorf("%w: router decision content identifier mismatch", ErrUpstream)
		}
		if routingRequest.Context.SchemaVersion == routingcontext.RiskSchemaVersion {
			if err := validateOptimizationDecision(*routingRequest.Context, decision, response.StatusCode); err != nil {
				return RoutingDecision{}, fmt.Errorf("%w: invalid optimized routing evidence", ErrUpstream)
			}
		}
	}
	if response.StatusCode == http.StatusUnprocessableEntity {
		if decision.Error == nil || decision.Error.Code != "no_eligible_provider" {
			return RoutingDecision{}, fmt.Errorf("%w: rejected routing decision omitted its error contract", ErrUpstream)
		}
		if expectedDigest != "" && (decision.SelectedCandidate != (RouteCandidate{}) || len(decision.CandidateOrder) != 0) {
			return RoutingDecision{}, fmt.Errorf("%w: rejected routing decision has a candidate", ErrUpstream)
		}
		if decision.Risk != nil {
			switch decision.Risk.Outcome {
			case "blocked":
				return decision, ErrRiskBlocked
			case "authentication_unsupported":
				return decision, ErrAuthenticationRequired
			case "requires_review":
				return decision, ErrRiskReviewRequired
			}
		}
		return decision, ErrNoEligibleProvider
	}
	if response.StatusCode != http.StatusOK {
		return RoutingDecision{}, fmt.Errorf("%w: router returned HTTP %d", ErrUpstream, response.StatusCode)
	}
	if decision.Error != nil {
		return RoutingDecision{}, fmt.Errorf("%w: successful routing decision carried an error envelope", ErrUpstream)
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
