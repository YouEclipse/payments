package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/YouEclipse/payments/services/payment/internal/identifier"
	"github.com/YouEclipse/payments/services/payment/internal/postgres"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

type Router interface {
	Decide(context.Context, clients.RoutingRequest) (clients.RoutingDecision, error)
}

type Gateway interface {
	Execute(context.Context, clients.GatewayOperationRequest) (domain.ProviderOutcome, error)
	Query(context.Context, clients.GatewayOperationIdentity) (domain.ProviderOutcome, error)
}

type Service struct {
	store           *postgres.Store
	router          Router
	gateway         Gateway
	logger          *slog.Logger
	upstreamTimeout time.Duration
	now             func() time.Time
	contextBuilder  *routingcontext.Builder
}

var ErrInvalidRequest = errors.New("invalid payment request")

type Option func(*Service)

func WithRoutingContext(builder *routingcontext.Builder) Option {
	return func(service *Service) { service.contextBuilder = builder }
}

func NewService(store *postgres.Store, router Router, gateway Gateway, logger *slog.Logger, upstreamTimeout time.Duration, options ...Option) *Service {
	service := &Service{
		store:           store,
		router:          router,
		gateway:         gateway,
		logger:          logger,
		upstreamTimeout: upstreamTimeout,
		now:             time.Now,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

type CreateIntentInput struct {
	MerchantReference  string `json:"merchant_reference"`
	AmountMinor        int64  `json:"amount_minor"`
	Currency           string `json:"currency"`
	PaymentMethodToken string `json:"payment_method_token"`
	CaptureMethod      string `json:"capture_method"`
}

type PaymentIntentView struct {
	ID                    string                 `json:"id"`
	Object                string                 `json:"object"`
	MerchantReference     string                 `json:"merchant_reference"`
	AmountMinor           int64                  `json:"amount_minor"`
	Currency              string                 `json:"currency"`
	PaymentMethodType     string                 `json:"payment_method_type"`
	CaptureMethod         domain.CaptureMethod   `json:"capture_method"`
	Status                domain.IntentStatus    `json:"status"`
	IntegrityStatus       domain.IntegrityStatus `json:"integrity_status"`
	AuthorizedAmountMinor int64                  `json:"authorized_amount_minor"`
	CapturedAmountMinor   int64                  `json:"captured_amount_minor"`
	RefundedAmountMinor   int64                  `json:"refunded_amount_minor"`
	LatestAttemptID       string                 `json:"latest_attempt_id,omitempty"`
	CreatedAt             time.Time              `json:"created_at"`
	UpdatedAt             time.Time              `json:"updated_at"`
}

type Result struct {
	Status      int
	Body        []byte
	Intent      PaymentIntentView
	Replay      bool
	Quarantined bool
}

func (s *Service) CreateIntent(ctx context.Context, merchantID, key string, input CreateIntentInput) (Result, error) {
	if err := idempotency.ValidateKey(key); err != nil {
		return Result{}, err
	}
	if input.CaptureMethod != string(domain.CaptureAutomatic) {
		return Result{}, fmt.Errorf("%w: only capture_method=automatic is supported", ErrInvalidRequest)
	}
	canonicalPayload, err := json.Marshal(input)
	if err != nil {
		return Result{}, fmt.Errorf("marshal create intent command: %w", err)
	}
	requestHash := idempotency.Fingerprint("create_payment_intent", canonicalPayload)
	intentID, err := identifier.New("pi_")
	if err != nil {
		return Result{}, err
	}
	now := s.now().UTC()
	intent, err := domain.NewIntent(intentID, merchantID, input.MerchantReference, input.AmountMinor, input.Currency, input.PaymentMethodToken, now)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	view := viewOf(intent)
	body, err := json.Marshal(view)
	if err != nil {
		return Result{}, fmt.Errorf("marshal payment intent: %w", err)
	}
	stored, err := s.store.CreateIntent(ctx, postgres.CreateIntentCommand{
		Intent:         intent,
		IdempotencyKey: key,
		Operation:      "create_payment_intent",
		RequestHash:    requestHash,
		Response:       postgres.StoredResponse{Status: http.StatusCreated, Body: body},
	})
	if err != nil {
		return Result{}, err
	}
	if stored.Replay != nil {
		return Result{Status: stored.Replay.Status, Body: stored.Replay.Body, Intent: viewOf(stored.Intent), Replay: true}, nil
	}
	return Result{Status: http.StatusCreated, Body: body, Intent: view}, nil
}

func (s *Service) ConfirmIntent(ctx context.Context, merchantID, intentID, key string) (Result, error) {
	if err := idempotency.ValidateKey(key); err != nil {
		return Result{}, err
	}
	operationName := "confirm_payment_intent:" + intentID
	requestHash := idempotency.Fingerprint(operationName, []byte(`{}`))
	lookup, err := s.store.LookupIdempotency(ctx, merchantID, key, operationName, requestHash)
	if err != nil {
		return Result{}, err
	}
	if lookup.Found {
		if !lookup.InProgress {
			return Result{Status: lookup.Response.Status, Body: lookup.Response.Body, Replay: true}, nil
		}
		intent, err := s.store.GetIntent(ctx, merchantID, lookup.ResourceID)
		if err != nil {
			return Result{}, err
		}
		return resultForIntent(intent, true)
	}

	intent, err := s.store.GetIntent(ctx, merchantID, intentID)
	if err != nil {
		return Result{}, err
	}
	if intent.Status != domain.IntentRequiresConfirmation {
		return Result{}, postgres.ErrStateConflict
	}
	attemptID, err := identifier.New("att_")
	if err != nil {
		return Result{}, err
	}
	operationID, err := identifier.New("op_")
	if err != nil {
		return Result{}, err
	}

	routeContext, cancelRoute := context.WithTimeout(ctx, s.upstreamTimeout)
	routingRequest, err := s.buildRoutingRequest(routeContext, intent, attemptID)
	if err != nil {
		cancelRoute()
		return Result{}, err
	}
	decision, err := s.router.Decide(routeContext, routingRequest)
	cancelRoute()
	if err != nil {
		return Result{}, err
	}

	var contextSnapshot, decisionSnapshot []byte
	if routingRequest.Context != nil {
		contextSnapshot, err = json.Marshal(routingRequest.Context)
		if err != nil {
			return Result{}, err
		}
		decisionSnapshot, err = json.Marshal(decision)
		if err != nil {
			return Result{}, err
		}
	}

	gatewayCommand := clients.GatewayOperationRequest{
		OperationID:                operationID,
		AttemptID:                  attemptID,
		ProviderConnectionID:       decision.SelectedCandidate.CandidateID,
		ProviderConnectionRevision: decision.SelectedCandidate.ConnectionRevision,
		ProviderNamespaceID:        decision.SelectedCandidate.ProviderNamespaceID,
		Provider:                   decision.SelectedCandidate.Provider,
		AmountMinor:                intent.AmountMinor,
		Currency:                   intent.Currency,
		PaymentMethodToken:         intent.PaymentMethodToken,
		CaptureMethod:              string(intent.CaptureMethod),
	}
	providerPayload, err := json.Marshal(gatewayCommand)
	if err != nil {
		return Result{}, err
	}
	now := s.now().UTC()
	prepared, err := s.store.PrepareConfirmation(ctx, postgres.PrepareConfirmationCommand{
		MerchantID:     merchantID,
		IdempotencyKey: key,
		OperationName:  operationName,
		RequestHash:    requestHash,
		Now:            now,
		Attempt: domain.PaymentAttempt{
			ID:                         attemptID,
			PaymentIntentID:            intent.ID,
			AttemptNumber:              1,
			RoutingDecisionID:          decision.DecisionID,
			RoutingContextSnapshot:     string(contextSnapshot),
			RoutingDecisionSnapshot:    string(decisionSnapshot),
			ProviderConnectionID:       decision.SelectedCandidate.CandidateID,
			ProviderConnectionRevision: decision.SelectedCandidate.ConnectionRevision,
			ProviderNamespaceID:        decision.SelectedCandidate.ProviderNamespaceID,
			Provider:                   decision.SelectedCandidate.Provider,
			RerouteDisposition:         domain.RerouteBlocked,
			CreatedAt:                  now,
			UpdatedAt:                  now,
		},
		Operation: domain.ProviderOperation{
			ID:                         operationID,
			PaymentIntentID:            intent.ID,
			PaymentAttemptID:           attemptID,
			Kind:                       domain.OperationSale,
			AmountMinor:                intent.AmountMinor,
			Currency:                   intent.Currency,
			ProviderConnectionID:       decision.SelectedCandidate.CandidateID,
			ProviderConnectionRevision: decision.SelectedCandidate.ConnectionRevision,
			ProviderNamespaceID:        decision.SelectedCandidate.ProviderNamespaceID,
			Provider:                   decision.SelectedCandidate.Provider,
			ProviderIdempotencyKey:     operationID,
			MerchantReference:          operationID,
			RequestHash:                idempotency.Fingerprint("provider_sale", providerPayload),
			CreatedAt:                  now,
			UpdatedAt:                  now,
		},
	})
	if err != nil {
		return Result{}, err
	}
	if prepared.Replay != nil {
		return Result{Status: prepared.Replay.Status, Body: prepared.Replay.Body, Replay: true}, nil
	}
	if prepared.InProgress {
		return resultForIntent(prepared.Aggregate.Intent, true)
	}

	aggregate, err := s.store.MarkOperationSending(ctx, operationID, s.now().UTC())
	if err != nil {
		// A durable worker may have claimed the operation between prepare and
		// this synchronous fast path. Return the authoritative current state.
		if errors.Is(err, domain.ErrInvalidTransition) {
			current, loadErr := s.store.GetIntent(ctx, merchantID, intentID)
			if loadErr != nil {
				return Result{}, loadErr
			}
			return s.finalizeResult(ctx, merchantID, key, current)
		}
		return Result{}, err
	}
	outcome := s.executeGateway(ctx, aggregate)
	applyResult, applyErr := s.applyOutcome(ctx, outcome)
	if applyErr != nil && !errors.Is(applyErr, domain.ErrConflictingOutcome) {
		return Result{}, applyErr
	}
	return s.finalizeResult(ctx, merchantID, key, applyResult.Aggregate.Intent)
}

func (s *Service) GetIntent(ctx context.Context, merchantID, intentID string) (Result, error) {
	intent, err := s.store.GetIntent(ctx, merchantID, intentID)
	if err != nil {
		return Result{}, err
	}
	return resultForRetrievedIntent(intent)
}

func (s *Service) ReceiveProviderOutcome(ctx context.Context, outcome domain.ProviderOutcome, sanitizedPayload []byte) (Result, bool, error) {
	if outcome.EventID == "" || outcome.ProviderConnectionID == "" || outcome.ProviderConnectionRevision == "" ||
		outcome.ProviderNamespaceID == "" || outcome.Provider == "" || (outcome.OperationID == "" && outcome.ProviderIdempotencyKey == "") {
		return Result{}, false, fmt.Errorf("%w: event_id, provider connection, provider and operation correlation are required", ErrInvalidRequest)
	}
	receivedAt := s.now().UTC()
	result, err := s.store.ApplyOutcome(ctx, outcome, sanitizedPayload, receivedAt, storedResponseForIntent)
	if err != nil && !errors.Is(err, domain.ErrConflictingOutcome) {
		return Result{}, false, err
	}
	if result.Rejected {
		s.logger.Warn("provider outcome quarantined",
			slog.String("provider", outcome.Provider),
			slog.String("event_id", outcome.EventID),
			slog.String("operation_id", outcome.OperationID),
		)
		return Result{Status: http.StatusAccepted, Intent: viewOf(result.Aggregate.Intent), Quarantined: true}, result.Duplicate, nil
	}
	publicResult, resultErr := resultForIntent(result.Aggregate.Intent, false)
	if resultErr != nil {
		return Result{}, false, resultErr
	}
	return publicResult, result.Duplicate, err
}

func (s *Service) executeGateway(ctx context.Context, aggregate postgres.Aggregate) domain.ProviderOutcome {
	callContext, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()
	outcome, err := s.gateway.Execute(callContext, clients.GatewayOperationRequest{
		OperationID:                aggregate.Operation.ID,
		AttemptID:                  aggregate.Attempt.ID,
		ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
		Provider:                   aggregate.Operation.Provider,
		AmountMinor:                aggregate.Operation.AmountMinor,
		Currency:                   aggregate.Operation.Currency,
		PaymentMethodToken:         aggregate.Intent.PaymentMethodToken,
		CaptureMethod:              string(aggregate.Intent.CaptureMethod),
	})
	if err == nil {
		outcome.EventID = newBestEffortEventID("sync_")
		return outcome
	}
	return gatewayFailureOutcome(aggregate, "sync_unknown_", "gateway_transport_unknown", err)
}

func (s *Service) queryGateway(ctx context.Context, aggregate postgres.Aggregate) domain.ProviderOutcome {
	callContext, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()
	outcome, err := s.gateway.Query(callContext, clients.GatewayOperationIdentity{
		OperationID:                aggregate.Operation.ID,
		AttemptID:                  aggregate.Attempt.ID,
		ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
		Provider:                   aggregate.Operation.Provider,
	})
	if err == nil {
		outcome.EventID = newBestEffortEventID("query_")
		return outcome
	}
	if errors.Is(err, clients.ErrGatewayOperationNotFound) {
		// Gateway uses 404 for an operation that is absent, or was durably created
		// but never became possibly dispatched. Resubmit the exact same operation
		// ID, provider, key and payload; Gateway verifies its durable request hash
		// before any call. This is not a new Attempt or cross-provider fallback.
		return s.executeGateway(ctx, aggregate)
	}
	return gatewayFailureOutcome(aggregate, "query_unknown_", "gateway_query_unknown", err)
}

func gatewayFailureOutcome(aggregate postgres.Aggregate, eventPrefix, code string, err error) domain.ProviderOutcome {
	outcome := domain.ProviderOutcome{
		EventID:                    newBestEffortEventID(eventPrefix),
		ProviderConnectionID:       aggregate.Operation.ProviderConnectionID,
		ProviderConnectionRevision: aggregate.Operation.ProviderConnectionRevision,
		ProviderNamespaceID:        aggregate.Operation.ProviderNamespaceID,
		Provider:                   aggregate.Operation.Provider,
		OperationID:                aggregate.Operation.ID,
		ProviderIdempotencyKey:     aggregate.Operation.ProviderIdempotencyKey,
		Status:                     domain.OutcomeUnknown,
		SideEffectCertainty:        domain.SideEffectPossible,
		RetryMode:                  domain.RetryQueryOrWait,
		NormalizedCode:             code,
		Message:                    err.Error(),
	}
	if errors.Is(err, clients.ErrGatewayIdentityMismatch) {
		outcome.RequiresReview = true
		outcome.RetryMode = domain.RetryNone
		outcome.NormalizedCode = "gateway_execution_identity_conflict"
	}
	return outcome
}

func (s *Service) applyOutcome(ctx context.Context, outcome domain.ProviderOutcome) (postgres.ApplyOutcomeResult, error) {
	payload, _ := json.Marshal(outcome)
	return s.store.ApplyOutcome(ctx, outcome, payload, s.now().UTC(), storedResponseForIntent)
}

func storedResponseForIntent(intent domain.PaymentIntent) (postgres.StoredResponse, error) {
	result, err := resultForIntent(intent, false)
	if err != nil {
		return postgres.StoredResponse{}, err
	}
	return postgres.StoredResponse{Status: result.Status, Body: result.Body}, nil
}

func (s *Service) finalizeResult(ctx context.Context, merchantID, key string, intent domain.PaymentIntent) (Result, error) {
	result, err := resultForIntent(intent, false)
	if err != nil {
		return Result{}, err
	}
	if err := s.store.CompleteIdempotency(ctx, merchantID, key, postgres.StoredResponse{Status: result.Status, Body: result.Body}, s.now().UTC()); err != nil {
		return Result{}, err
	}
	return result, nil
}

func resultForIntent(intent domain.PaymentIntent, replay bool) (Result, error) {
	view := viewOf(intent)
	body, err := json.Marshal(view)
	if err != nil {
		return Result{}, fmt.Errorf("marshal payment intent response: %w", err)
	}
	status := http.StatusOK
	if intent.Status == domain.IntentProcessing || intent.Status == domain.IntentRequiresAction {
		status = http.StatusAccepted
	}
	return Result{Status: status, Body: body, Intent: view, Replay: replay}, nil
}

func resultForRetrievedIntent(intent domain.PaymentIntent) (Result, error) {
	result, err := resultForIntent(intent, false)
	if err != nil {
		return Result{}, err
	}
	// GET reports the current representation successfully even while the
	// underlying payment is still processing. HTTP 202 is reserved for an
	// accepted mutation such as confirm.
	result.Status = http.StatusOK
	return result, nil
}

func viewOf(intent domain.PaymentIntent) PaymentIntentView {
	return PaymentIntentView{
		ID:                    intent.ID,
		Object:                "payment_intent",
		MerchantReference:     intent.MerchantReference,
		AmountMinor:           intent.AmountMinor,
		Currency:              intent.Currency,
		PaymentMethodType:     intent.PaymentMethodType,
		CaptureMethod:         intent.CaptureMethod,
		Status:                intent.Status,
		IntegrityStatus:       intent.IntegrityStatus,
		AuthorizedAmountMinor: intent.AuthorizedAmountMinor,
		CapturedAmountMinor:   intent.CapturedAmountMinor,
		RefundedAmountMinor:   intent.RefundedAmountMinor,
		LatestAttemptID:       intent.LatestAttemptID,
		CreatedAt:             intent.CreatedAt,
		UpdatedAt:             intent.UpdatedAt,
	}
}

func newBestEffortEventID(prefix string) string {
	id, err := identifier.New(prefix)
	if err == nil {
		return id
	}
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}
