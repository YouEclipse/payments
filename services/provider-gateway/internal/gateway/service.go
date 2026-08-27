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
	Environment                 string
	ProviderKinds               map[string]string
	ProviderConnectionRevisions map[string]string
	ProviderNamespaceIDs        map[string]string
	ProviderRecoveryAliases     map[string]string
	ProviderConclusiveAbsence   map[string]bool
	ProviderEndpoints           map[string]string
	WebhookBaseURL              string
	WebhookSecrets              map[string]string
	ProviderTimeout             time.Duration
	PaymentOutcomeURL           string
	InternalToken               string
}

type Service struct {
	cfg                   Config
	store                 Store
	configuredConnections map[string]ProviderConnectionIdentity
	configuredRevisions   map[string]ProviderConnectionRevisionBinding
	client                *http.Client
	log                   *slog.Logger
}

const durableStoreTimeout = 2 * time.Second

var (
	ErrProviderOperationNotFound          = errors.New("provider operation not found")
	ErrResubmitRequired                   = errors.New("provider operation requires same-key resubmission")
	ErrProviderIdentityMismatch           = errors.New("provider response identity mismatch")
	ErrProviderConnectionIdentityMismatch = errors.New("provider connection identity does not match configuration")
	ErrInvalidOperation                   = errors.New("invalid provider operation")
	ErrInvalidWebhook                     = errors.New("invalid provider webhook")
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
	cfg.ProviderKinds = cloneStringMap(cfg.ProviderKinds)
	cfg.ProviderConnectionRevisions = cloneStringMap(cfg.ProviderConnectionRevisions)
	cfg.ProviderNamespaceIDs = cloneStringMap(cfg.ProviderNamespaceIDs)
	cfg.ProviderRecoveryAliases = cloneStringMap(cfg.ProviderRecoveryAliases)
	cfg.ProviderConclusiveAbsence = cloneBoolMap(cfg.ProviderConclusiveAbsence)
	cfg.ProviderEndpoints = cloneStringMap(cfg.ProviderEndpoints)
	cfg.WebhookSecrets = cloneStringMap(cfg.WebhookSecrets)
	if len(cfg.ProviderEndpoints) == 0 {
		return nil, errors.New("at least one provider endpoint is required")
	}
	configuredConnections := make(map[string]ProviderConnectionIdentity, len(cfg.ProviderEndpoints))
	configuredRevisions := make(map[string]ProviderConnectionRevisionBinding, len(cfg.ProviderEndpoints))
	namespaceProviders := make(map[string]string, len(cfg.ProviderEndpoints))
	for connectionID, endpoint := range cfg.ProviderEndpoints {
		trimmedConnectionID := strings.TrimSpace(connectionID)
		provider := strings.TrimSpace(cfg.ProviderKinds[connectionID])
		revision := strings.TrimSpace(cfg.ProviderConnectionRevisions[connectionID])
		namespaceID := strings.TrimSpace(cfg.ProviderNamespaceIDs[connectionID])
		if trimmedConnectionID == "" || trimmedConnectionID != connectionID || provider == "" || provider != cfg.ProviderKinds[connectionID] ||
			revision == "" || revision != cfg.ProviderConnectionRevisions[connectionID] ||
			namespaceID == "" || namespaceID != cfg.ProviderNamespaceIDs[connectionID] ||
			strings.TrimSpace(endpoint) == "" || strings.TrimSpace(endpoint) != endpoint {
			return nil, errors.New("provider connection IDs, revisions, namespace IDs, provider kinds, and endpoints must be explicit, non-empty, and must not contain surrounding whitespace")
		}
		if strings.TrimSpace(cfg.WebhookSecrets[connectionID]) == "" {
			return nil, fmt.Errorf("webhook secret is required for provider connection %q", connectionID)
		}
		normalizedEndpoint, err := normalizeProviderEndpoint(endpoint)
		if err != nil {
			return nil, fmt.Errorf("provider connection %q endpoint: %w", connectionID, err)
		}
		if !IsDevelopmentEnvironment(cfg.Environment) && !strings.HasPrefix(normalizedEndpoint, "https://") {
			return nil, fmt.Errorf("provider connection %q endpoint must use HTTPS outside local, development, and test environments", connectionID)
		}
		cfg.ProviderEndpoints[connectionID] = normalizedEndpoint
		if !IsDevelopmentEnvironment(cfg.Environment) && strings.HasPrefix(strings.ToLower(provider), "simulator") {
			return nil, errors.New("simulator providers are forbidden outside local, development, and test environments")
		}
		if configuredProvider, exists := namespaceProviders[namespaceID]; exists && configuredProvider != provider {
			return nil, fmt.Errorf("provider namespace %q cannot contain both provider %q and provider %q", namespaceID, configuredProvider, provider)
		}
		namespaceProviders[namespaceID] = provider
		configuredConnections[connectionID] = ProviderConnectionIdentity{
			Provider: provider, ConnectionRevision: revision, NamespaceID: namespaceID,
		}
		configuredRevisions[connectionID] = connectionRevisionBinding(
			configuredConnections[connectionID], normalizedEndpoint, cfg.WebhookSecrets[connectionID],
			cfg.ProviderConclusiveAbsence[connectionID],
		)
	}
	for mapName, configured := range map[string]map[string]string{
		"PROVIDER_CONNECTION_PROVIDERS": cfg.ProviderKinds,
		"PROVIDER_CONNECTION_REVISIONS": cfg.ProviderConnectionRevisions,
		"PROVIDER_NAMESPACE_IDS":        cfg.ProviderNamespaceIDs,
		"PROVIDER_WEBHOOK_SECRETS":      cfg.WebhookSecrets,
	} {
		for connectionID := range configured {
			if _, exists := cfg.ProviderEndpoints[connectionID]; !exists {
				return nil, fmt.Errorf("%s configures unknown provider connection %q", mapName, connectionID)
			}
		}
	}
	for connectionID := range cfg.ProviderConclusiveAbsence {
		if _, exists := cfg.ProviderEndpoints[connectionID]; !exists {
			return nil, fmt.Errorf("PROVIDER_CONCLUSIVE_ABSENCE configures unknown provider connection %q", connectionID)
		}
	}
	for aliasConnectionID, targetConnectionID := range cfg.ProviderRecoveryAliases {
		aliasIdentity, aliasExists := configuredConnections[aliasConnectionID]
		targetIdentity, targetExists := configuredConnections[targetConnectionID]
		if !aliasExists || !targetExists || aliasConnectionID == targetConnectionID {
			return nil, fmt.Errorf("provider recovery alias %q must reference a different configured connection", aliasConnectionID)
		}
		if aliasIdentity.ConnectionRevision != legacyConnectionRevision ||
			aliasConnectionID != aliasIdentity.NamespaceID || aliasConnectionID != aliasIdentity.Provider {
			return nil, fmt.Errorf("provider recovery alias %q must use revision %q and its legacy provider namespace", aliasConnectionID, legacyConnectionRevision)
		}
		if aliasIdentity.Provider != targetIdentity.Provider || aliasIdentity.NamespaceID != targetIdentity.NamespaceID {
			return nil, fmt.Errorf("provider recovery alias %q and target %q must share provider and namespace", aliasConnectionID, targetConnectionID)
		}
		if cfg.ProviderEndpoints[aliasConnectionID] != cfg.ProviderEndpoints[targetConnectionID] ||
			cfg.WebhookSecrets[aliasConnectionID] != cfg.WebhookSecrets[targetConnectionID] {
			return nil, fmt.Errorf("provider recovery alias %q and target %q must share endpoint and webhook secret", aliasConnectionID, targetConnectionID)
		}
		if _, chained := cfg.ProviderRecoveryAliases[targetConnectionID]; chained {
			return nil, fmt.Errorf("provider recovery alias %q cannot target another alias", aliasConnectionID)
		}
	}
	webhookBaseURL, err := normalizeGatewayURL(cfg.WebhookBaseURL, !IsDevelopmentEnvironment(cfg.Environment))
	if err != nil {
		return nil, fmt.Errorf("webhook base URL: %w", err)
	}
	cfg.WebhookBaseURL = webhookBaseURL
	if !IsDevelopmentEnvironment(cfg.Environment) {
		if strings.TrimSpace(cfg.InternalToken) == "" {
			return nil, errors.New("internal service token is required outside local, development, and test environments")
		}
		if strings.TrimSpace(cfg.PaymentOutcomeURL) == "" {
			return nil, errors.New("payment outcome URL is required outside local, development, and test environments")
		}
	}
	if cfg.PaymentOutcomeURL != "" {
		paymentOutcomeURL, err := normalizeGatewayURL(cfg.PaymentOutcomeURL, !IsDevelopmentEnvironment(cfg.Environment))
		if err != nil {
			return nil, fmt.Errorf("payment outcome URL: %w", err)
		}
		cfg.PaymentOutcomeURL = paymentOutcomeURL
	}
	validationCtx, cancel := context.WithTimeout(context.Background(), durableStoreTimeout)
	defer cancel()
	if err := store.ValidateNonterminalConnections(validationCtx, configuredConnections); err != nil {
		return nil, fmt.Errorf("validate durable provider connections: %w", err)
	}
	if err := store.BindConnectionRevisions(validationCtx, configuredRevisions); err != nil {
		return nil, fmt.Errorf("bind immutable provider connection revisions: %w", err)
	}
	if cfg.ProviderTimeout <= 0 {
		cfg.ProviderTimeout = 750 * time.Millisecond
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:                   cfg,
		store:                 store,
		configuredConnections: configuredConnections,
		configuredRevisions:   configuredRevisions,
		client:                &http.Client{Timeout: 5 * time.Second},
		log:                   log,
	}, nil
}

func (s *Service) Ready(ctx context.Context) error {
	if err := s.store.Ping(ctx); err != nil {
		return err
	}
	if err := s.store.ValidateNonterminalConnections(ctx, s.configuredConnections); err != nil {
		return err
	}
	return s.store.BindConnectionRevisions(ctx, s.configuredRevisions)
}

func (s *Service) Execute(ctx context.Context, req OperationRequest) (Operation, error) {
	if err := s.validateRequestShape(req); err != nil {
		return Operation{}, fmt.Errorf("%w: %v", ErrInvalidOperation, err)
	}
	if err := s.validateConfiguredRequestIdentity(req); err != nil {
		existing, getErr := s.store.Get(ctx, req.OperationID)
		if getErr == nil {
			if !operationRequestIdentityMatches(existing, req) {
				return Operation{}, ErrIdempotencyConflict
			}
			return Operation{}, fmt.Errorf("%w: %v", ErrUnconfiguredConnection, err)
		}
		if !errors.Is(getErr, ErrNotFound) {
			return Operation{}, getErr
		}
		return Operation{}, fmt.Errorf("%w: %w", ErrInvalidOperation, err)
	}
	if _, recoveryOnly := s.cfg.ProviderRecoveryAliases[req.ProviderConnectionID]; recoveryOnly {
		existing, err := s.store.Get(ctx, req.OperationID)
		if errors.Is(err, ErrNotFound) {
			return Operation{}, fmt.Errorf("%w: recovery-only provider connections cannot create new operations", ErrInvalidOperation)
		}
		if err != nil {
			return Operation{}, err
		}
		if !operationRequestIdentityMatches(existing, req) {
			return Operation{}, ErrIdempotencyConflict
		}
	}
	now := time.Now().UTC()
	op := Operation{
		OperationID:                req.OperationID,
		AttemptID:                  req.AttemptID,
		ProviderConnectionID:       req.ProviderConnectionID,
		ProviderConnectionRevision: req.ProviderConnectionRevision,
		ProviderNamespaceID:        req.ProviderNamespaceID,
		Provider:                   req.Provider,
		ProviderIdempotencyKey:     req.OperationID,
		RequestHash:                operationHashV2(req),
		RequestHashVersion:         requestHashVersionConnection,
		LegacyRequestHash:          operationHashV1(req),
		Status:                     StatusCreated,
		SideEffectCertainty:        "none",
		CreatedAt:                  now,
		UpdatedAt:                  now,
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
	if err := s.validateStoredOperationIdentity(op); err != nil {
		return Operation{}, err
	}
	if !operationRequestIdentityMatches(op, req) {
		return Operation{}, ErrIdempotencyConflict
	}
	if !time.Now().UTC().Before(op.CreatedAt.Add(localRecoveryWindow)) {
		return s.setStatusDurably(ctx, op.OperationID, StatusReview, "possible", "", "local_recovery_window_exhausted")
	}
	marked, claimed, err := s.claimDispatchDurably(ctx, op)
	if err != nil {
		return Operation{}, err
	}
	// Seeing Sending is not ownership: another worker may hold that state. Only
	// the compare-and-swap winner may cross the Provider boundary.
	if !claimed {
		return marked, nil
	}
	op = marked

	providerCtx, cancel := context.WithTimeout(ctx, s.cfg.ProviderTimeout)
	defer cancel()
	result, err := s.sale(providerCtx, req, op)
	if err != nil {
		// Once dispatch may have crossed the process boundary, transport errors are not proof of failure.
		current, _, storeErr := s.applyProviderObservationDurably(ctx, &op,
			providerCallErrorObservation(op.OperationID, ObservationSourceSale, "provider_response_unknown", err))
		return current, storeErr
	}
	if err := validateProviderResponseIdentity(result, req.Provider, req.OperationID); err != nil {
		s.log.Error("provider sale response identity mismatch",
			"operation_id", req.OperationID,
			"provider", req.Provider,
			"response_operation_id", result.OperationID,
			"response_provider", result.Provider,
		)
		current, _, storeErr := s.applyProviderObservationDurably(ctx, nil, providerResponseObservation(
			op.OperationID, ObservationSourceSale, result, StatusReview, "possible", "", "provider_response_identity_mismatch",
		))
		return current, storeErr
	}
	status, certainty, failureCode := normalizeProviderResult(result.Status, result.ProviderReference, result.FailureCode)
	if status == StatusUnknown {
		current, _, storeErr := s.applyProviderObservationDurably(ctx, &op, providerResponseObservation(
			op.OperationID, ObservationSourceSale, result, status, certainty, result.ProviderReference, failureCode,
		))
		return current, storeErr
	}
	current, _, storeErr := s.applyProviderObservationDurably(ctx, nil, providerResponseObservation(
		op.OperationID, ObservationSourceSale, result, status, certainty, result.ProviderReference, failureCode,
	))
	return current, storeErr
}

func (s *Service) reconcileBeforeResubmit(ctx context.Context, req OperationRequest, op Operation) (Operation, error) {
	inFlight, err := s.store.DispatchInFlight(ctx, op.OperationID)
	if err != nil {
		return Operation{}, err
	}
	if inFlight {
		return op, nil
	}
	result, err := s.queryProviderWithTimeout(ctx, op)
	if errors.Is(err, ErrUnconfiguredConnection) {
		return Operation{}, err
	}
	if errors.Is(err, ErrProviderOperationNotFound) {
		// A conclusive provider-side not-found permits resubmitting only the exact
		// same logical operation. CreateOrGet already verified the full request
		// hash, including provider, token, amount and capture method.
		current, applied, storeErr := s.applyProviderObservationDurably(ctx, &op, providerAbsenceObservation(op.OperationID))
		if storeErr != nil || !applied {
			return current, storeErr
		}
		return s.dispatch(ctx, req, current)
	}
	if err != nil {
		current, _, storeErr := s.applyProviderObservationDurably(ctx, &op,
			providerCallErrorObservation(op.OperationID, ObservationSourceQuery, "provider_query_unknown", err))
		return current, storeErr
	}
	return s.applyProviderResponse(ctx, op, result, ObservationSourceQuery)
}

func (s *Service) Get(ctx context.Context, operationID string, reconcile bool) (Operation, error) {
	op, err := s.store.Get(ctx, operationID)
	if err != nil || !reconcile || op.Status == StatusSucceeded || op.Status == StatusFailed || op.Status == StatusReview {
		return op, err
	}
	if err := s.validateStoredOperationIdentity(op); err != nil {
		return Operation{}, err
	}
	if op.Status == StatusCreated {
		return Operation{}, ErrResubmitRequired
	}
	inFlight, err := s.store.DispatchInFlight(ctx, op.OperationID)
	if err != nil {
		return Operation{}, err
	}
	if inFlight {
		return op, nil
	}
	if op.Status != StatusSending && op.Status != StatusUnknown {
		return op, nil
	}
	result, err := s.queryProviderWithTimeout(ctx, op)
	if errors.Is(err, ErrProviderOperationNotFound) {
		current, applied, storeErr := s.applyProviderObservationDurably(ctx, &op, providerAbsenceObservation(op.OperationID))
		if storeErr != nil {
			return Operation{}, storeErr
		}
		if !applied {
			return current, nil
		}
		return Operation{}, ErrResubmitRequired
	}
	if err != nil {
		current, _, storeErr := s.applyProviderObservationDurably(ctx, &op,
			providerCallErrorObservation(op.OperationID, ObservationSourceQuery, "provider_query_unknown", err))
		return current, storeErr
	}
	return s.applyProviderResponse(ctx, op, result, ObservationSourceQuery)
}

func (s *Service) queryProviderWithTimeout(ctx context.Context, op Operation) (providerResponse, error) {
	if err := s.validateStoredOperationIdentity(op); err != nil {
		return providerResponse{}, err
	}
	providerCtx, cancel := context.WithTimeout(ctx, s.cfg.ProviderTimeout)
	defer cancel()
	return s.queryProvider(providerCtx, op)
}

func (s *Service) applyProviderResponse(ctx context.Context, op Operation, result providerResponse, source string) (Operation, error) {
	if err := validateProviderResponseIdentity(result, op.Provider, op.OperationID); err != nil {
		s.log.Error("provider query response identity mismatch",
			"operation_id", op.OperationID,
			"provider", op.Provider,
			"response_operation_id", result.OperationID,
			"response_provider", result.Provider,
		)
		current, _, storeErr := s.applyProviderObservationDurably(ctx, nil, providerResponseObservation(
			op.OperationID, source, result, StatusReview, "possible", "", "provider_response_identity_mismatch",
		))
		return current, storeErr
	}
	status, certainty, failureCode := normalizeProviderResult(result.Status, result.ProviderReference, result.FailureCode)
	if status == StatusUnknown {
		// A nonterminal Provider observation belongs to the exact query/dispatch
		// generation that produced it. A late authorization must not clear a
		// newer dispatch lease and admit another Provider POST.
		current, _, storeErr := s.applyProviderObservationDurably(ctx, &op, providerResponseObservation(
			op.OperationID, source, result, status, certainty, result.ProviderReference, failureCode,
		))
		return current, storeErr
	}
	current, _, storeErr := s.applyProviderObservationDurably(ctx, nil, providerResponseObservation(
		op.OperationID, source, result, status, certainty, result.ProviderReference, failureCode,
	))
	return current, storeErr
}

func (s *Service) setStatusDurably(parent context.Context, operationID, status, certainty, reference, failureCode string) (Operation, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), durableStoreTimeout)
	defer cancel()
	return s.store.SetStatus(ctx, operationID, status, certainty, reference, failureCode)
}

func (s *Service) applyProviderObservationDurably(
	parent context.Context,
	expected *Operation,
	observation ProviderOperationObservation,
) (Operation, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), durableStoreTimeout)
	defer cancel()
	return s.store.ApplyProviderObservation(ctx, observation, expected)
}

func (s *Service) claimDispatchDurably(parent context.Context, expected Operation) (Operation, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), durableStoreTimeout)
	defer cancel()
	return s.store.ClaimDispatch(ctx, expected, s.cfg.ProviderTimeout+durableStoreTimeout)
}

func (s *Service) ApplyWebhook(ctx context.Context, providerConnectionID string, body []byte, signature string) (bool, error) {
	secret, ok := s.cfg.WebhookSecrets[providerConnectionID]
	if !ok || secret == "" {
		return false, fmt.Errorf("%w: unknown provider connection", ErrInvalidWebhook)
	}
	pathIdentity, ok := s.configuredConnections[providerConnectionID]
	if !ok {
		return false, fmt.Errorf("%w: incomplete provider connection configuration", ErrInvalidWebhook)
	}
	provider := pathIdentity.Provider
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
	if err := decodeProviderJSONBytes(body, &event); err != nil {
		return false, fmt.Errorf("%w: malformed JSON", ErrInvalidWebhook)
	}
	if event.EventID == "" || len(event.EventID) > 256 ||
		event.OperationID == "" || len(event.OperationID) > 256 ||
		event.Provider == "" || len(event.Provider) > 128 || event.Provider != provider ||
		len(event.ProviderReference) > 512 || len(event.FailureCode) > 128 || event.OccurredAt.IsZero() {
		return false, fmt.Errorf("%w: event identity is incomplete or inconsistent", ErrInvalidWebhook)
	}
	switch event.Status {
	case "authorized", "captured", "succeeded", "declined", "failed":
	default:
		return false, fmt.Errorf("%w: unsupported provider status", ErrInvalidWebhook)
	}
	status, certainty, failureCode := normalizeProviderResult(event.Status, event.ProviderReference, event.FailureCode)
	verifiedEvent := VerifiedWebhookEvent{
		PathConnectionID:           providerConnectionID,
		PathIdentity:               pathIdentity,
		AllowedOperationIdentities: map[string]ProviderConnectionIdentity{providerConnectionID: pathIdentity},
		EventID:                    event.EventID,
		OperationID:                event.OperationID,
		RawProviderStatus:          event.Status,
		Status:                     status,
		SideEffectCertainty:        certainty,
		ProviderReference:          event.ProviderReference,
		FailureCode:                failureCode,
		OccurredAt:                 event.OccurredAt,
		SanitizedPayload:           mustJSON(event),
	}
	if targetConnectionID, isRecoveryAlias := s.cfg.ProviderRecoveryAliases[providerConnectionID]; isRecoveryAlias {
		verifiedEvent.AllowedOperationIdentities[targetConnectionID] = s.configuredConnections[targetConnectionID]
	}
	_, inserted, err := s.store.ApplyWebhook(ctx, verifiedEvent)
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

func (s *Service) sale(ctx context.Context, input OperationRequest, op Operation) (providerResponse, error) {
	endpoint := strings.TrimRight(s.cfg.ProviderEndpoints[op.ProviderConnectionID], "/") + "/v1/sales"
	body, _ := json.Marshal(map[string]any{
		"operation_id": input.OperationID, "amount_minor": input.AmountMinor, "currency": input.Currency,
		"payment_method_token": input.PaymentMethodToken, "capture_method": input.CaptureMethod,
		"scenario": input.Scenario, "webhook_url": strings.TrimRight(s.cfg.WebhookBaseURL, "/") + "/webhooks/" + url.PathEscape(op.ProviderConnectionID),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providerResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", op.ProviderIdempotencyKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return providerResponse{}, err
	}
	defer resp.Body.Close()
	responseBody, err := readProviderResponseBody(resp)
	if err != nil {
		return providerResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providerResponse{}, newProviderCallDiagnosticError("http_error", resp.StatusCode, responseBody, errors.New("non-success HTTP status"))
	}
	var result providerResponse
	if err := decodeProviderJSONBytes(responseBody, &result); err != nil {
		return providerResponse{}, newProviderCallDiagnosticError("malformed_response", resp.StatusCode, responseBody, err)
	}
	return result, nil
}

func (s *Service) queryProvider(ctx context.Context, op Operation) (providerResponse, error) {
	endpoint := strings.TrimRight(s.cfg.ProviderEndpoints[op.ProviderConnectionID], "/") + "/v1/sales/" + url.PathEscape(op.ProviderIdempotencyKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return providerResponse{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return providerResponse{}, err
	}
	defer resp.Body.Close()
	responseBody, err := readProviderResponseBody(resp)
	if err != nil {
		return providerResponse{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		// A structured body is conclusive only when this exact connection revision
		// durably declares the capability. A proxy 404, malformed body, or another
		// revision's Provider-brand promise cannot prove that money never moved.
		var missing struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		binding, bound := s.configuredRevisions[op.ProviderConnectionID]
		if bound && binding.ConnectionRevision == op.ProviderConnectionRevision && binding.ConclusiveAbsence &&
			decodeProviderJSONBytes(responseBody, &missing) == nil && missing.Error.Code == "operation_not_found" {
			return providerResponse{}, ErrProviderOperationNotFound
		}
		return providerResponse{}, newProviderCallDiagnosticError(
			"unproven_not_found", resp.StatusCode, responseBody, errors.New("provider query returned an unproven HTTP 404"),
		)
	}
	if resp.StatusCode != http.StatusOK {
		return providerResponse{}, newProviderCallDiagnosticError("http_error", resp.StatusCode, responseBody, errors.New("non-success HTTP status"))
	}
	var result providerResponse
	if err := decodeProviderJSONBytes(responseBody, &result); err != nil {
		return providerResponse{}, newProviderCallDiagnosticError("malformed_response", resp.StatusCode, responseBody, err)
	}
	return result, nil
}

const maxProviderResponseBytes = 64 << 10

func readProviderResponseBody(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		return nil, newProviderCallDiagnosticError("body_read_error", response.StatusCode, body, err)
	}
	if len(body) > maxProviderResponseBytes {
		return nil, newTruncatedProviderCallDiagnosticError(
			"oversized_response", response.StatusCode, body, errors.New("provider response exceeded the bounded diagnostic limit"),
		)
	}
	return body, nil
}

func newProviderCallDiagnosticError(category string, statusCode int, body []byte, cause error) error {
	return newProviderCallDiagnosticErrorWithTruncation(category, statusCode, body, false, cause)
}

func newTruncatedProviderCallDiagnosticError(category string, statusCode int, observedPrefix []byte, cause error) error {
	return newProviderCallDiagnosticErrorWithTruncation(category, statusCode, observedPrefix, true, cause)
}

func newProviderCallDiagnosticErrorWithTruncation(
	category string,
	statusCode int,
	body []byte,
	truncated bool,
	cause error,
) error {
	hash := sha256.Sum256(body)
	return &providerCallDiagnosticError{
		category: category, statusCode: statusCode,
		bodyHash: hex.EncodeToString(hash[:]), byteCount: len(body), truncated: truncated, cause: cause,
	}
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

func (s *Service) validateRequestShape(req OperationRequest) error {
	for _, value := range []string{
		req.OperationID, req.AttemptID, req.ProviderConnectionID, req.ProviderConnectionRevision,
		req.ProviderNamespaceID, req.Provider, req.Currency, req.PaymentMethodToken, req.CaptureMethod, req.Scenario,
	} {
		if strings.ContainsRune(value, '\x00') {
			return errors.New("operation request fields must not contain NUL bytes")
		}
	}
	if req.OperationID == "" || req.AttemptID == "" {
		return errors.New("operation_id and attempt_id are required")
	}
	if req.ProviderConnectionID == "" || req.ProviderConnectionRevision == "" || req.ProviderNamespaceID == "" || req.Provider == "" {
		return errors.New("provider_connection_id, provider_connection_revision, provider_namespace_id, and provider are required")
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

func (s *Service) validateConfiguredRequestIdentity(req OperationRequest) error {
	configured, ok := s.configuredConnections[req.ProviderConnectionID]
	if !ok {
		return fmt.Errorf("%w: unknown provider connection", ErrProviderConnectionIdentityMismatch)
	}
	if configured.Provider != req.Provider ||
		configured.ConnectionRevision != req.ProviderConnectionRevision ||
		configured.NamespaceID != req.ProviderNamespaceID {
		return ErrProviderConnectionIdentityMismatch
	}
	return nil
}

func (s *Service) validateStoredOperationIdentity(op Operation) error {
	configured, ok := s.configuredConnections[op.ProviderConnectionID]
	if !ok || configured.Provider != op.Provider ||
		configured.ConnectionRevision != op.ProviderConnectionRevision ||
		configured.NamespaceID != op.ProviderNamespaceID {
		return fmt.Errorf("%w: connection=%q revision=%q namespace=%q provider=%q",
			ErrUnconfiguredConnection, op.ProviderConnectionID, op.ProviderConnectionRevision, op.ProviderNamespaceID, op.Provider)
	}
	return nil
}

func operationRequestIdentityMatches(op Operation, req OperationRequest) bool {
	return op.ProviderConnectionID == req.ProviderConnectionID &&
		op.ProviderConnectionRevision == req.ProviderConnectionRevision &&
		op.ProviderNamespaceID == req.ProviderNamespaceID &&
		op.Provider == req.Provider
}

func cloneStringMap(input map[string]string) map[string]string {
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneBoolMap(input map[string]bool) map[string]bool {
	cloned := make(map[string]bool, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func normalizeProviderEndpoint(raw string) (string, error) {
	return normalizeGatewayURL(raw, false)
}

// normalizeGatewayURL gives execution and registry code one URL identity.
// Development may use HTTP for the local simulator; every non-development
// funds or callback boundary is authenticated by TLS before any token or
// internal service credential can cross it.
func normalizeGatewayURL(raw string, requireHTTPS bool) (string, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("URL must be non-empty and must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("endpoint must be an absolute HTTP or HTTPS URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Opaque != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("endpoint must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	if requireHTTPS && parsed.Scheme != "https" {
		return "", errors.New("URL must use HTTPS outside local, development, and test environments")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	if port := parsed.Port(); (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		parsed.Host = strings.TrimSuffix(parsed.Host, ":"+port)
	}
	// Dispatch already appends a fixed API path after trimming trailing slashes.
	// The registry fingerprints the same normalized URL that execution uses.
	return strings.TrimRight(parsed.String(), "/"), nil
}

func connectionRevisionBinding(identity ProviderConnectionIdentity, normalizedEndpoint, webhookSecret string, conclusiveAbsence bool) ProviderConnectionRevisionBinding {
	secretDigest := sha256.Sum256([]byte(webhookSecret))
	material := struct {
		Version             int    `json:"version"`
		Provider            string `json:"provider"`
		NamespaceID         string `json:"provider_namespace_id"`
		Endpoint            string `json:"endpoint"`
		WebhookSecretDigest string `json:"webhook_secret_digest"`
	}{
		Version: executionFingerprintVersion, Provider: identity.Provider, NamespaceID: identity.NamespaceID,
		Endpoint: normalizedEndpoint, WebhookSecretDigest: hex.EncodeToString(secretDigest[:]),
	}
	// Real adapters must extend this versioned material with immutable account,
	// region and provider credential-scope references before they are enabled.
	// The simulator adapter has no separate account or outbound credential.
	fingerprint := sha256.Sum256(mustJSON(material))
	return ProviderConnectionRevisionBinding{
		ProviderConnectionIdentity: identity, FingerprintVersion: executionFingerprintVersion,
		ConfigFingerprint: hex.EncodeToString(fingerprint[:]), ConclusiveAbsence: conclusiveAbsence,
	}
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
		// An adapter protocol value outside the explicit allowlist is drift, not
		// a recoverable transport ambiguity. Stop automatic execution for review.
		return StatusReview, "possible"
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
	if normalizedStatus == StatusReview && strings.TrimSpace(failureCode) == "" {
		failureCode = "unsupported_provider_status"
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

func operationHashV1(req OperationRequest) string {
	material := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s", req.OperationID, req.AttemptID, req.Provider, req.AmountMinor, req.Currency, req.PaymentMethodToken, req.CaptureMethod, req.Scenario)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func operationHashV2(req OperationRequest) string {
	material := struct {
		Version                    int    `json:"version"`
		OperationID                string `json:"operation_id"`
		AttemptID                  string `json:"attempt_id"`
		ProviderConnectionID       string `json:"provider_connection_id"`
		ProviderConnectionRevision string `json:"provider_connection_revision"`
		ProviderNamespaceID        string `json:"provider_namespace_id"`
		Provider                   string `json:"provider"`
		AmountMinor                int64  `json:"amount_minor"`
		Currency                   string `json:"currency"`
		PaymentMethodToken         string `json:"payment_method_token"`
		CaptureMethod              string `json:"capture_method"`
		Scenario                   string `json:"scenario"`
	}{
		Version: requestHashVersionConnection, OperationID: req.OperationID, AttemptID: req.AttemptID,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionRevision: req.ProviderConnectionRevision,
		ProviderNamespaceID: req.ProviderNamespaceID, Provider: req.Provider,
		AmountMinor: req.AmountMinor, Currency: req.Currency, PaymentMethodToken: req.PaymentMethodToken,
		CaptureMethod: req.CaptureMethod, Scenario: req.Scenario,
	}
	// A fixed JSON struct is unambiguous even when token or simulator scenario
	// strings contain delimiter characters; v1 remains byte-for-byte compatible.
	sum := sha256.Sum256(mustJSON(material))
	return hex.EncodeToString(sum[:])
}
