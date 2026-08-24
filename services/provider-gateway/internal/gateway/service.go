package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	Environment       string
	ProviderEndpoints map[string]string
	WebhookBaseURL    string
	WebhookSecrets    map[string]string
	ProviderTimeout   time.Duration
	PaymentOutcomeURL string
	InternalToken     string
}

type Service struct {
	cfg    Config
	store  Store
	client *http.Client
	log    *slog.Logger
}

const durableStoreTimeout = 2 * time.Second

var (
	ErrProviderOperationNotFound = errors.New("provider operation not found")
	ErrResubmitRequired          = errors.New("provider operation requires same-key resubmission")
	ErrProviderIdentityMismatch  = errors.New("provider response identity mismatch")
	ErrInvalidOperation          = errors.New("invalid provider operation")
	ErrInvalidWebhook            = errors.New("invalid provider webhook")
)

// NormalizeEnvironment canonicalizes the small, explicit set of deployment
// environments understood by the gateway. Unknown values fail closed instead
// of inheriting local credentials or simulator defaults.
func NormalizeEnvironment(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "local":
		return "local", nil
	case "dev", "development":
		return "development", nil
	case "test":
		return "test", nil
	case "stage", "staging":
		return "staging", nil
	case "prod", "production":
		return "production", nil
	case "live":
		return "live", nil
	default:
		return "", fmt.Errorf("unsupported PAYMENTS_ENVIRONMENT %q", raw)
	}
}

func IsDevelopmentEnvironment(environment string) bool {
	switch environment {
	case "local", "development", "test":
		return true
	default:
		return false
	}
}

func NewService(cfg Config, store Store, log *slog.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	environment, err := NormalizeEnvironment(cfg.Environment)
	if err != nil {
		return nil, err
	}
	cfg.Environment = environment
	if len(cfg.ProviderEndpoints) == 0 {
		return nil, errors.New("at least one provider endpoint is required")
	}
	for provider, endpoint := range cfg.ProviderEndpoints {
		trimmedProvider := strings.TrimSpace(provider)
		if trimmedProvider == "" || trimmedProvider != provider || strings.TrimSpace(endpoint) == "" || strings.TrimSpace(endpoint) != endpoint {
			return nil, errors.New("provider names and endpoints must be non-empty and must not contain surrounding whitespace")
		}
		if strings.TrimSpace(cfg.WebhookSecrets[provider]) == "" {
			return nil, fmt.Errorf("webhook secret is required for provider %q", provider)
		}
		if !IsDevelopmentEnvironment(cfg.Environment) && strings.HasPrefix(strings.ToLower(trimmedProvider), "simulator_") {
			return nil, errors.New("simulator providers are forbidden outside local, development, and test environments")
		}
	}
	if cfg.WebhookBaseURL == "" {
		return nil, errors.New("webhook base URL is required")
	}
	if !IsDevelopmentEnvironment(cfg.Environment) {
		if strings.TrimSpace(cfg.InternalToken) == "" {
			return nil, errors.New("internal service token is required outside local, development, and test environments")
		}
		if strings.TrimSpace(cfg.PaymentOutcomeURL) == "" {
			return nil, errors.New("payment outcome URL is required outside local, development, and test environments")
		}
	}
	if cfg.ProviderTimeout <= 0 {
		cfg.ProviderTimeout = 750 * time.Millisecond
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, store: store, client: &http.Client{Timeout: 5 * time.Second}, log: log}, nil
}

func (s *Service) Execute(ctx context.Context, req OperationRequest) (Operation, error) {
	if err := s.validate(req); err != nil {
		return Operation{}, fmt.Errorf("%w: %v", ErrInvalidOperation, err)
	}
	now := time.Now().UTC()
	op := Operation{
		OperationID:            req.OperationID,
		AttemptID:              req.AttemptID,
		Provider:               req.Provider,
		ProviderIdempotencyKey: req.OperationID,
		RequestHash:            operationHash(req),
		Status:                 StatusCreated,
		SideEffectCertainty:    "none",
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	stored, created, err := s.store.CreateOrGet(ctx, op)
	if err != nil {
		return stored, err
	}
	if created || stored.Status == StatusCreated {
		return s.dispatch(ctx, req, stored)
	}
	if stored.Status == StatusSucceeded || stored.Status == StatusFailed || stored.Status == StatusReview {
		return stored, nil
	}
	if stored.Status == StatusSending || stored.Status == StatusUnknown {
		return s.reconcileBeforeResubmit(ctx, req, stored)
	}
	return Operation{}, fmt.Errorf("unsupported stored operation status %q", stored.Status)
}

func (s *Service) dispatch(ctx context.Context, req OperationRequest, op Operation) (Operation, error) {
	if _, err := s.setStatusDurably(ctx, op.OperationID, StatusSending, "possible", "", ""); err != nil {
		return Operation{}, err
	}

	providerCtx, cancel := context.WithTimeout(ctx, s.cfg.ProviderTimeout)
	defer cancel()
	result, err := s.sale(providerCtx, req, op.ProviderIdempotencyKey)
	if err != nil {
		// Once dispatch may have crossed the process boundary, transport errors are not proof of failure.
		return s.setStatusDurably(ctx, op.OperationID, StatusUnknown, "possible", "", "provider_response_unknown")
	}
	if err := validateProviderResponseIdentity(result, req.Provider, req.OperationID); err != nil {
		s.log.Error("provider sale response identity mismatch",
			"operation_id", req.OperationID,
			"provider", req.Provider,
			"response_operation_id", result.OperationID,
			"response_provider", result.Provider,
		)
		return s.setStatusDurably(ctx, op.OperationID, StatusReview, "possible", "", "provider_response_identity_mismatch")
	}
	status, certainty, failureCode := normalizeProviderResult(result.Status, result.ProviderReference, result.FailureCode)
	return s.setStatusDurably(ctx, op.OperationID, status, certainty, result.ProviderReference, failureCode)
}

func (s *Service) reconcileBeforeResubmit(ctx context.Context, req OperationRequest, op Operation) (Operation, error) {
	result, err := s.queryProviderWithTimeout(ctx, op)
	if errors.Is(err, ErrProviderOperationNotFound) {
		// A conclusive provider-side not-found permits resubmitting only the exact
		// same logical operation. CreateOrGet already verified the full request
		// hash, including provider, token, amount and capture method.
		return s.dispatch(ctx, req, op)
	}
	if err != nil {
		return s.setStatusDurably(ctx, op.OperationID, StatusUnknown, "possible", "", "provider_query_unknown")
	}
	return s.applyProviderResponse(ctx, op, result)
}

func (s *Service) Get(ctx context.Context, operationID string, reconcile bool) (Operation, error) {
	op, err := s.store.Get(ctx, operationID)
	if err != nil || !reconcile || op.Status == StatusSucceeded || op.Status == StatusFailed || op.Status == StatusReview {
		return op, err
	}
	if op.Status == StatusCreated {
		return Operation{}, ErrResubmitRequired
	}
	if op.Status != StatusSending && op.Status != StatusUnknown {
		return op, nil
	}
	result, err := s.queryProviderWithTimeout(ctx, op)
	if errors.Is(err, ErrProviderOperationNotFound) {
		return Operation{}, ErrResubmitRequired
	}
	if err != nil {
		return s.setStatusDurably(ctx, op.OperationID, StatusUnknown, "possible", "", "provider_query_unknown")
	}
	return s.applyProviderResponse(ctx, op, result)
}

func (s *Service) queryProviderWithTimeout(ctx context.Context, op Operation) (providerResponse, error) {
	providerCtx, cancel := context.WithTimeout(ctx, s.cfg.ProviderTimeout)
	defer cancel()
	return s.queryProvider(providerCtx, op)
}

func (s *Service) applyProviderResponse(ctx context.Context, op Operation, result providerResponse) (Operation, error) {
	if err := validateProviderResponseIdentity(result, op.Provider, op.OperationID); err != nil {
		s.log.Error("provider query response identity mismatch",
			"operation_id", op.OperationID,
			"provider", op.Provider,
			"response_operation_id", result.OperationID,
			"response_provider", result.Provider,
		)
		return s.setStatusDurably(ctx, op.OperationID, StatusReview, "possible", "", "provider_response_identity_mismatch")
	}
	status, certainty, failureCode := normalizeProviderResult(result.Status, result.ProviderReference, result.FailureCode)
	return s.setStatusDurably(ctx, op.OperationID, status, certainty, result.ProviderReference, failureCode)
}

func (s *Service) setStatusDurably(parent context.Context, operationID, status, certainty, reference, failureCode string) (Operation, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), durableStoreTimeout)
	defer cancel()
	return s.store.SetStatus(ctx, operationID, status, certainty, reference, failureCode)
}

func (s *Service) ApplyWebhook(ctx context.Context, provider string, body []byte, signature string) (bool, error) {
	secret, ok := s.cfg.WebhookSecrets[provider]
	if !ok || secret == "" {
		return false, fmt.Errorf("%w: unknown provider", ErrInvalidWebhook)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	actual, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(expected, actual) {
		return false, fmt.Errorf("%w: signature verification failed", ErrInvalidWebhook)
	}
	var event struct {
		EventID           string    `json:"event_id"`
		Provider          string    `json:"provider"`
		OperationID       string    `json:"operation_id"`
		ProviderReference string    `json:"provider_reference"`
		Status            string    `json:"status"`
		FailureCode       string    `json:"failure_code"`
		OccurredAt        time.Time `json:"occurred_at"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return false, fmt.Errorf("%w: malformed JSON", ErrInvalidWebhook)
	}
	if event.EventID == "" || event.OperationID == "" || event.Provider != provider {
		return false, fmt.Errorf("%w: event identity is incomplete or inconsistent", ErrInvalidWebhook)
	}
	switch event.Status {
	case "authorized", "captured", "succeeded", "declined", "failed":
	default:
		return false, fmt.Errorf("%w: unsupported provider status", ErrInvalidWebhook)
	}
	status, certainty, failureCode := normalizeProviderResult(event.Status, event.ProviderReference, event.FailureCode)
	outcome := ProviderOutcome{
		EventID:             event.EventID,
		OperationID:         event.OperationID,
		Provider:            provider,
		Status:              status,
		SideEffectCertainty: certainty,
		ProviderReference:   event.ProviderReference,
		FailureCode:         failureCode,
		OccurredAt:          event.OccurredAt,
	}
	_, inserted, err := s.store.ApplyWebhook(ctx, outcome)
	return inserted, err
}

func (s *Service) RunOutbox(ctx context.Context) {
	if s.cfg.PaymentOutcomeURL == "" {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dispatchOutboxBatch(ctx)
		}
	}
}

func (s *Service) dispatchOutboxBatch(ctx context.Context) {
	items, err := s.store.PendingOutbox(ctx, 50)
	if err != nil {
		s.log.Error("load gateway outbox", "error", err)
		return
	}
	for _, item := range items {
		if err := s.deliverOutcome(ctx, item.Outcome); err != nil {
			s.log.Warn("deliver provider outcome", "operation_id", item.Outcome.OperationID, "error", err)
			// Leave this item pending, but do not let one merchant/provider poison
			// the rest of the batch.
			continue
		}
		if err := s.store.MarkOutboxDelivered(ctx, item.ID); err != nil {
			s.log.Error("mark gateway outbox delivered", "outbox_id", item.ID, "error", err)
			continue
		}
	}
}

func (s *Service) sale(ctx context.Context, input OperationRequest, key string) (providerResponse, error) {
	endpoint := strings.TrimRight(s.cfg.ProviderEndpoints[input.Provider], "/") + "/v1/sales"
	body, _ := json.Marshal(map[string]any{
		"operation_id": input.OperationID, "amount_minor": input.AmountMinor, "currency": input.Currency,
		"payment_method_token": input.PaymentMethodToken, "capture_method": input.CaptureMethod,
		"scenario": input.Scenario, "webhook_url": strings.TrimRight(s.cfg.WebhookBaseURL, "/") + "/webhooks/" + url.PathEscape(input.Provider),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providerResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := s.client.Do(req)
	if err != nil {
		return providerResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return providerResponse{}, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	var result providerResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return providerResponse{}, err
	}
	return result, nil
}

func (s *Service) queryProvider(ctx context.Context, op Operation) (providerResponse, error) {
	endpoint := strings.TrimRight(s.cfg.ProviderEndpoints[op.Provider], "/") + "/v1/sales/" + url.PathEscape(op.ProviderIdempotencyKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return providerResponse{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return providerResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return providerResponse{}, ErrProviderOperationNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return providerResponse{}, fmt.Errorf("provider query returned HTTP %d", resp.StatusCode)
	}
	var result providerResponse
	return result, json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result)
}

func (s *Service) deliverOutcome(ctx context.Context, outcome ProviderOutcome) error {
	body, _ := json.Marshal(outcome)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.PaymentOutcomeURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", s.cfg.InternalToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("payment service returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Service) validate(req OperationRequest) error {
	if req.OperationID == "" || req.AttemptID == "" {
		return errors.New("operation_id and attempt_id are required")
	}
	if _, ok := s.cfg.ProviderEndpoints[req.Provider]; !ok {
		return errors.New("unknown provider")
	}
	if req.AmountMinor <= 0 || len(req.Currency) != 3 || strings.ToUpper(req.Currency) != req.Currency {
		return errors.New("amount_minor and uppercase ISO currency are required")
	}
	if req.PaymentMethodToken == "" {
		return errors.New("payment_method_token is required")
	}
	if req.CaptureMethod != "automatic" {
		return errors.New("phase one supports capture_method=automatic only")
	}
	if req.Scenario != "" && s.cfg.Environment != "local" && s.cfg.Environment != "development" && s.cfg.Environment != "test" {
		return errors.New("test scenarios are disabled outside local, development, and test")
	}
	return nil
}

func normalizeProviderStatus(status string) (string, string) {
	switch status {
	case "captured", "succeeded":
		return StatusSucceeded, "confirmed"
	case "authorized":
		// An authorization hold is not proof that an automatic-capture SALE has
		// captured funds. Keep reconciling until capture is explicit.
		return StatusUnknown, "possible"
	case "declined", "failed":
		return StatusFailed, "none"
	default:
		return StatusUnknown, "possible"
	}
}

func normalizeProviderResult(status, providerReference, failureCode string) (string, string, string) {
	normalizedStatus, certainty := normalizeProviderStatus(status)
	if status == "authorized" {
		if strings.TrimSpace(providerReference) == "" {
			return StatusReview, "possible", "authorization_reference_missing"
		}
		if strings.TrimSpace(failureCode) == "" {
			failureCode = "authorization_only"
		}
	}
	if normalizedStatus == StatusSucceeded && strings.TrimSpace(providerReference) == "" {
		return StatusReview, "possible", "provider_reference_missing"
	}
	return normalizedStatus, certainty, failureCode
}

func validateProviderResponseIdentity(response providerResponse, expectedProvider, expectedOperationID string) error {
	if response.Provider != expectedProvider || response.OperationID != expectedOperationID {
		return fmt.Errorf("%w: expected provider=%q operation_id=%q, got provider=%q operation_id=%q",
			ErrProviderIdentityMismatch,
			expectedProvider,
			expectedOperationID,
			response.Provider,
			response.OperationID,
		)
	}
	return nil
}

func operationHash(req OperationRequest) string {
	material := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s", req.OperationID, req.AttemptID, req.Provider, req.AmountMinor, req.Currency, req.PaymentMethodToken, req.CaptureMethod, req.Scenario)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}
