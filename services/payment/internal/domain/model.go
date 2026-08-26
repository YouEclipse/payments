package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type IntentStatus string

const (
	IntentRequiresConfirmation IntentStatus = "requires_confirmation"
	IntentProcessing           IntentStatus = "processing"
	IntentRequiresAction       IntentStatus = "requires_action"
	IntentSucceeded            IntentStatus = "succeeded"
	IntentFailed               IntentStatus = "failed"
	IntentCanceled             IntentStatus = "canceled"
)

type IntegrityStatus string

const (
	IntegrityHealthy        IntegrityStatus = "healthy"
	IntegrityReviewRequired IntegrityStatus = "review_required"
)

type CaptureMethod string

const (
	CaptureAutomatic CaptureMethod = "automatic"
)

type AttemptStatus string

const (
	AttemptCreated    AttemptStatus = "created"
	AttemptProcessing AttemptStatus = "processing"
	AttemptUnknown    AttemptStatus = "unknown"
	AttemptCaptured   AttemptStatus = "captured"
	AttemptDeclined   AttemptStatus = "declined"
	AttemptFailed     AttemptStatus = "failed"
)

type RerouteDisposition string

const (
	RerouteBlocked RerouteDisposition = "blocked"
	RerouteAllowed RerouteDisposition = "allowed"
)

type OperationKind string

const (
	// OperationSale is a provider-native automatic-capture payment. It is not
	// equivalent to an authorization-only hold.
	OperationSale OperationKind = "sale"
)

type OperationStatus string

const (
	OperationCreated        OperationStatus = "created"
	OperationSending        OperationStatus = "sending"
	OperationRetryScheduled OperationStatus = "retry_scheduled"
	OperationPending        OperationStatus = "pending"
	OperationUnknown        OperationStatus = "unknown"
	OperationReviewRequired OperationStatus = "review_required"
	OperationSucceeded      OperationStatus = "succeeded"
	OperationFailed         OperationStatus = "failed"
)

type SideEffectCertainty string

const (
	SideEffectNone      SideEffectCertainty = "none"
	SideEffectPossible  SideEffectCertainty = "possible"
	SideEffectConfirmed SideEffectCertainty = "confirmed"
)

type RetryMode string

const (
	RetrySameKey     RetryMode = "same_key"
	RetryQueryOrWait RetryMode = "query_or_wait"
	RetryNone        RetryMode = "none"
)

type OutcomeStatus string

const (
	OutcomeSucceeded OutcomeStatus = "succeeded"
	OutcomeDeclined  OutcomeStatus = "declined"
	OutcomeFailed    OutcomeStatus = "failed"
	OutcomePending   OutcomeStatus = "pending"
	OutcomeUnknown   OutcomeStatus = "unknown"
)

var (
	ErrInvalidTransition  = errors.New("invalid payment state transition")
	ErrConflictingOutcome = errors.New("provider outcome conflicts with a terminal result")
)

type PaymentIntent struct {
	ID                    string
	MerchantID            string
	MerchantReference     string
	AmountMinor           int64
	Currency              string
	PaymentMethodType     string
	PaymentMethodToken    string
	CaptureMethod         CaptureMethod
	Status                IntentStatus
	IntegrityStatus       IntegrityStatus
	AuthorizedAmountMinor int64
	CapturedAmountMinor   int64
	RefundedAmountMinor   int64
	ActiveAttemptID       string
	LatestAttemptID       string
	WinningAttemptID      string
	Version               int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type PaymentAttempt struct {
	ID                         string
	PaymentIntentID            string
	AttemptNumber              int
	RoutingDecisionID          string
	RoutingContextSnapshot     string
	RoutingDecisionSnapshot    string
	ProviderConnectionID       string
	ProviderConnectionRevision string
	ProviderNamespaceID        string
	Provider                   string
	Status                     AttemptStatus
	OutcomeCode                string
	RerouteDisposition         RerouteDisposition
	ProviderPaymentReference   string
	Version                    int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

type ProviderOperation struct {
	ID                         string
	PaymentIntentID            string
	PaymentAttemptID           string
	Kind                       OperationKind
	AmountMinor                int64
	Currency                   string
	ProviderConnectionID       string
	ProviderConnectionRevision string
	ProviderNamespaceID        string
	Provider                   string
	ProviderIdempotencyKey     string
	MerchantReference          string
	RequestHash                []byte
	Status                     OperationStatus
	SideEffectCertainty        SideEffectCertainty
	RetryMode                  RetryMode
	ProviderOperationReference string
	NormalizedCode             string
	ProviderCode               string
	RetryCount                 int
	NextRetryAt                *time.Time
	LastError                  string
	Version                    int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	CompletedAt                *time.Time
}

type ProviderOutcome struct {
	EventID                    string
	ProviderConnectionID       string
	ProviderConnectionRevision string
	ProviderNamespaceID        string
	Provider                   string
	OperationID                string
	ProviderIdempotencyKey     string
	Status                     OutcomeStatus
	SideEffectCertainty        SideEffectCertainty
	RetryMode                  RetryMode
	ProviderPaymentReference   string
	ProviderOperationReference string
	NormalizedCode             string
	ProviderCode               string
	Message                    string
	RequiresReview             bool
	OccurredAt                 time.Time
}

type ApplyResult struct {
	Changed       bool
	IntentChanged bool
	Conflict      bool
}

func NewIntent(id, merchantID, merchantReference string, amountMinor int64, currency, token string, now time.Time) (PaymentIntent, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(merchantID) == "" {
		return PaymentIntent{}, errors.New("intent and merchant IDs are required")
	}
	if strings.TrimSpace(merchantReference) == "" || strings.TrimSpace(merchantReference) != merchantReference || len(merchantReference) > 128 {
		return PaymentIntent{}, errors.New("merchant reference must be 1 to 128 non-whitespace-surrounded bytes")
	}
	if amountMinor <= 0 {
		return PaymentIntent{}, errors.New("amount_minor must be greater than zero")
	}
	if len(currency) != 3 || currency != strings.ToUpper(currency) {
		return PaymentIntent{}, errors.New("currency must be a three-letter uppercase code")
	}
	if strings.TrimSpace(token) == "" || strings.TrimSpace(token) != token || len(token) > 512 {
		return PaymentIntent{}, errors.New("payment method token must be 1 to 512 non-whitespace-surrounded bytes")
	}

	return PaymentIntent{
		ID:                 id,
		MerchantID:         merchantID,
		MerchantReference:  merchantReference,
		AmountMinor:        amountMinor,
		Currency:           currency,
		PaymentMethodType:  "card",
		PaymentMethodToken: token,
		CaptureMethod:      CaptureAutomatic,
		Status:             IntentRequiresConfirmation,
		IntegrityStatus:    IntegrityHealthy,
		CreatedAt:          now.UTC(),
		UpdatedAt:          now.UTC(),
	}, nil
}

func PrepareConfirmation(intent *PaymentIntent, attempt *PaymentAttempt, operation *ProviderOperation, now time.Time) error {
	if intent.Status != IntentRequiresConfirmation {
		return fmt.Errorf("%w: intent %s cannot be confirmed from %s", ErrInvalidTransition, intent.ID, intent.Status)
	}
	if intent.ActiveAttemptID != "" || intent.WinningAttemptID != "" {
		return fmt.Errorf("%w: intent already has an active or winning attempt", ErrInvalidTransition)
	}
	if attempt.PaymentIntentID != intent.ID || operation.PaymentIntentID != intent.ID || operation.PaymentAttemptID != attempt.ID {
		return errors.New("attempt and operation do not belong to the intent")
	}
	if operation.Kind != OperationSale || intent.CaptureMethod != CaptureAutomatic {
		return errors.New("the first release only supports automatic-capture sale operations")
	}
	if operation.AmountMinor != intent.AmountMinor || operation.Currency != intent.Currency {
		return errors.New("operation amount and currency must equal the immutable intent")
	}
	if attempt.ProviderConnectionID == "" || operation.ProviderConnectionID != attempt.ProviderConnectionID {
		return errors.New("attempt and operation provider connection must match")
	}
	if attempt.ProviderConnectionRevision == "" || operation.ProviderConnectionRevision != attempt.ProviderConnectionRevision {
		return errors.New("attempt and operation provider connection revision must match")
	}
	if attempt.ProviderNamespaceID == "" || operation.ProviderNamespaceID != attempt.ProviderNamespaceID {
		return errors.New("attempt and operation provider namespace must match")
	}
	if attempt.Provider == "" || operation.Provider != attempt.Provider {
		return errors.New("attempt and operation provider kind must match")
	}
	if operation.ProviderIdempotencyKey == "" || len(operation.RequestHash) == 0 {
		return errors.New("provider idempotency key and request hash are required")
	}

	intent.Status = IntentProcessing
	intent.ActiveAttemptID = attempt.ID
	intent.LatestAttemptID = attempt.ID
	intent.UpdatedAt = now.UTC()
	intent.Version++
	attempt.Status = AttemptCreated
	attempt.RerouteDisposition = RerouteBlocked
	attempt.UpdatedAt = now.UTC()
	operation.Status = OperationCreated
	operation.SideEffectCertainty = SideEffectNone
	operation.RetryMode = RetrySameKey
	operation.UpdatedAt = now.UTC()
	return nil
}

func MarkSending(intent *PaymentIntent, attempt *PaymentAttempt, operation *ProviderOperation, now time.Time) error {
	if operation.Status != OperationCreated && operation.Status != OperationRetryScheduled && operation.Status != OperationUnknown {
		return fmt.Errorf("%w: operation %s cannot send from %s", ErrInvalidTransition, operation.ID, operation.Status)
	}
	if isTerminalAttempt(attempt.Status) || intent.WinningAttemptID != "" {
		return fmt.Errorf("%w: a terminal or winning attempt cannot be sent again", ErrInvalidTransition)
	}
	operation.Status = OperationSending
	operation.SideEffectCertainty = SideEffectPossible
	operation.RetryMode = RetryQueryOrWait
	operation.RetryCount++
	operation.NextRetryAt = nil
	operation.UpdatedAt = now.UTC()
	operation.Version++
	attempt.Status = AttemptProcessing
	attempt.UpdatedAt = now.UTC()
	attempt.Version++
	intent.Status = IntentProcessing
	intent.UpdatedAt = now.UTC()
	intent.Version++
	return nil
}

func ApplyProviderOutcome(intent *PaymentIntent, attempt *PaymentAttempt, operation *ProviderOperation, outcome ProviderOutcome, now time.Time) (ApplyResult, error) {
	if operation.PaymentIntentID != intent.ID || operation.PaymentAttemptID != attempt.ID {
		return ApplyResult{}, errors.New("operation aggregate does not match")
	}
	if outcome.ProviderConnectionID != operation.ProviderConnectionID {
		return ApplyResult{}, errors.New("provider outcome does not match operation provider connection")
	}
	if outcome.ProviderConnectionRevision != operation.ProviderConnectionRevision {
		return ApplyResult{}, errors.New("provider outcome does not match operation provider connection revision")
	}
	if outcome.ProviderNamespaceID != operation.ProviderNamespaceID {
		return ApplyResult{}, errors.New("provider outcome does not match operation provider namespace")
	}
	if outcome.Provider != operation.Provider {
		return ApplyResult{}, errors.New("provider outcome does not match operation provider kind")
	}
	if outcome.OperationID != "" && outcome.OperationID != operation.ID {
		return ApplyResult{}, errors.New("provider outcome operation ID does not match")
	}
	if outcome.ProviderIdempotencyKey != "" && outcome.ProviderIdempotencyKey != operation.ProviderIdempotencyKey {
		return ApplyResult{}, errors.New("provider idempotency key does not match")
	}
	switch outcome.Status {
	case OutcomeSucceeded, OutcomeDeclined, OutcomeFailed, OutcomePending, OutcomeUnknown:
	default:
		return ApplyResult{}, fmt.Errorf("unsupported provider outcome %q", outcome.Status)
	}

	beforeIntent := *intent
	beforeAttempt := *attempt
	beforeOperation := cloneOperation(*operation)
	finish := func(conflict bool, transitionErr error) (ApplyResult, error) {
		intentChanged := !sameIntentState(beforeIntent, *intent)
		attemptChanged := !sameAttemptState(beforeAttempt, *attempt)
		operationChanged := !sameOperationState(beforeOperation, *operation)
		if intentChanged {
			intent.Version = beforeIntent.Version + 1
			intent.UpdatedAt = now.UTC()
		} else {
			intent.Version = beforeIntent.Version
			intent.UpdatedAt = beforeIntent.UpdatedAt
		}
		if attemptChanged {
			attempt.Version = beforeAttempt.Version + 1
			attempt.UpdatedAt = now.UTC()
		} else {
			attempt.Version = beforeAttempt.Version
			attempt.UpdatedAt = beforeAttempt.UpdatedAt
		}
		if operationChanged {
			operation.Version = beforeOperation.Version + 1
			operation.UpdatedAt = now.UTC()
		} else {
			operation.Version = beforeOperation.Version
			operation.UpdatedAt = beforeOperation.UpdatedAt
		}
		return ApplyResult{
			Changed:       intentChanged || attemptChanged || operationChanged,
			IntentChanged: intentChanged,
			Conflict:      conflict,
		}, transitionErr
	}
	if operation.Status == OperationReviewRequired {
		intent.IntegrityStatus = IntegrityReviewRequired
		return finish(true, ErrConflictingOutcome)
	}

	if outcome.ProviderPaymentReference != "" {
		if attempt.ProviderPaymentReference != "" && attempt.ProviderPaymentReference != outcome.ProviderPaymentReference {
			intent.IntegrityStatus = IntegrityReviewRequired
			return finish(true, ErrConflictingOutcome)
		}
		if attempt.ProviderPaymentReference == "" {
			attempt.ProviderPaymentReference = outcome.ProviderPaymentReference
		}
	}
	if outcome.ProviderOperationReference != "" {
		if operation.ProviderOperationReference != "" && operation.ProviderOperationReference != outcome.ProviderOperationReference {
			intent.IntegrityStatus = IntegrityReviewRequired
			return finish(true, ErrConflictingOutcome)
		}
		if operation.ProviderOperationReference == "" {
			operation.ProviderOperationReference = outcome.ProviderOperationReference
		}
	}
	if outcome.RequiresReview {
		applyManualReviewState(
			intent,
			attempt,
			operation,
			firstNonEmpty(outcome.NormalizedCode, outcome.ProviderCode, "provider_review_required"),
			outcome.Message,
		)
		return finish(true, ErrConflictingOutcome)
	}

	if operation.Status == OperationSucceeded {
		if outcome.Status == OutcomeSucceeded || outcome.Status == OutcomePending || outcome.Status == OutcomeUnknown {
			if attempt.ProviderPaymentReference == "" || operation.ProviderOperationReference == "" {
				intent.IntegrityStatus = IntegrityReviewRequired
				return finish(true, ErrConflictingOutcome)
			}
			return finish(false, nil)
		}
		intent.IntegrityStatus = IntegrityReviewRequired
		return finish(true, ErrConflictingOutcome)
	}
	if operation.Status == OperationFailed {
		if outcome.Status == OutcomeDeclined || outcome.Status == OutcomeFailed {
			return finish(false, nil)
		}
		if outcome.Status == OutcomePending || outcome.Status == OutcomeUnknown {
			return finish(false, nil)
		}
		intent.IntegrityStatus = IntegrityReviewRequired
		return finish(true, ErrConflictingOutcome)
	}

	operation.ProviderCode = outcome.ProviderCode
	operation.NormalizedCode = outcome.NormalizedCode
	operation.LastError = outcome.Message

	switch outcome.Status {
	case OutcomeSucceeded:
		// A SALE cannot be considered captured unless it has a durable PSP
		// reference. Without that correlation key the only safe state is UNKNOWN
		// plus manual review; otherwise refunds and reconciliation are impossible.
		if attempt.ProviderPaymentReference == "" || operation.ProviderOperationReference == "" {
			intent.IntegrityStatus = IntegrityReviewRequired
			applyUnknown(intent, attempt, operation, now, false)
			return finish(true, ErrConflictingOutcome)
		}
		if intent.WinningAttemptID != "" && intent.WinningAttemptID != attempt.ID {
			operation.Status = OperationSucceeded
			operation.SideEffectCertainty = SideEffectConfirmed
			attempt.Status = AttemptCaptured
			intent.IntegrityStatus = IntegrityReviewRequired
			return finish(true, ErrConflictingOutcome)
		}
		completedAt := now.UTC()
		operation.Status = OperationSucceeded
		operation.SideEffectCertainty = SideEffectConfirmed
		operation.RetryMode = RetryNone
		operation.NextRetryAt = nil
		operation.CompletedAt = &completedAt
		attempt.Status = AttemptCaptured
		attempt.OutcomeCode = firstNonEmpty(outcome.NormalizedCode, "captured")
		attempt.RerouteDisposition = RerouteBlocked
		intent.Status = IntentSucceeded
		intent.AuthorizedAmountMinor = intent.AmountMinor
		intent.CapturedAmountMinor = intent.AmountMinor
		intent.ActiveAttemptID = ""
		intent.WinningAttemptID = attempt.ID
		return finish(false, nil)

	case OutcomeDeclined:
		completedAt := now.UTC()
		operation.Status = OperationFailed
		operation.SideEffectCertainty = SideEffectNone
		operation.RetryMode = RetryNone
		operation.NextRetryAt = nil
		operation.CompletedAt = &completedAt
		attempt.Status = AttemptDeclined
		attempt.OutcomeCode = firstNonEmpty(outcome.NormalizedCode, "declined")
		attempt.RerouteDisposition = RerouteBlocked
		intent.Status = IntentFailed
		intent.ActiveAttemptID = ""
		return finish(false, nil)

	case OutcomeFailed:
		if outcome.SideEffectCertainty != SideEffectNone {
			applyUnknown(intent, attempt, operation, now, true)
			return finish(false, nil)
		}
		completedAt := now.UTC()
		operation.Status = OperationFailed
		operation.SideEffectCertainty = SideEffectNone
		operation.RetryMode = RetryNone
		operation.NextRetryAt = nil
		operation.CompletedAt = &completedAt
		attempt.Status = AttemptFailed
		attempt.OutcomeCode = firstNonEmpty(outcome.NormalizedCode, "provider_failure")
		// First-release policy has no cross-provider fallback. A later policy may
		// set this to allowed only for an explicit no-side-effect allowlist.
		attempt.RerouteDisposition = RerouteBlocked
		intent.Status = IntentFailed
		intent.ActiveAttemptID = ""
		return finish(false, nil)

	case OutcomePending:
		operation.Status = OperationPending
		operation.SideEffectCertainty = SideEffectPossible
		operation.RetryMode = RetryQueryOrWait
		scheduleRecovery(operation, now)
		attempt.Status = AttemptProcessing
		intent.Status = IntentProcessing
		return finish(false, nil)

	case OutcomeUnknown:
		applyUnknown(intent, attempt, operation, now, true)
		return finish(false, nil)

	}
	return ApplyResult{}, fmt.Errorf("unsupported provider outcome %q", outcome.Status)
}

// RequireManualReview stops automatic recovery after evidence that cannot be
// safely assigned to this payment, such as a provider reference already owned
// by another attempt. Confirmed terminal facts are preserved; nonterminal work
// becomes review_required and must be resolved by an operator or a future
// explicit repair command.
func RequireManualReview(intent *PaymentIntent, attempt *PaymentAttempt, operation *ProviderOperation, code, message string, now time.Time) (ApplyResult, error) {
	if operation.PaymentIntentID != intent.ID || operation.PaymentAttemptID != attempt.ID || attempt.PaymentIntentID != intent.ID {
		return ApplyResult{}, errors.New("operation aggregate does not match")
	}
	beforeIntent := *intent
	beforeAttempt := *attempt
	beforeOperation := cloneOperation(*operation)

	applyManualReviewState(intent, attempt, operation, code, message)

	intentChanged := !sameIntentState(beforeIntent, *intent)
	attemptChanged := !sameAttemptState(beforeAttempt, *attempt)
	operationChanged := !sameOperationState(beforeOperation, *operation)
	if intentChanged {
		intent.Version = beforeIntent.Version + 1
		intent.UpdatedAt = now.UTC()
	}
	if attemptChanged {
		attempt.Version = beforeAttempt.Version + 1
		attempt.UpdatedAt = now.UTC()
	}
	if operationChanged {
		operation.Version = beforeOperation.Version + 1
		operation.UpdatedAt = now.UTC()
	}
	return ApplyResult{
		Changed:       intentChanged || attemptChanged || operationChanged,
		IntentChanged: intentChanged,
		Conflict:      true,
	}, nil
}

func applyManualReviewState(intent *PaymentIntent, attempt *PaymentAttempt, operation *ProviderOperation, code, message string) {
	intent.IntegrityStatus = IntegrityReviewRequired
	if !isTerminalAttempt(attempt.Status) {
		attempt.Status = AttemptUnknown
		attempt.RerouteDisposition = RerouteBlocked
	}
	if operation.Status != OperationSucceeded && operation.Status != OperationFailed {
		operation.Status = OperationReviewRequired
		operation.SideEffectCertainty = SideEffectPossible
		operation.RetryMode = RetryNone
		operation.NextRetryAt = nil
		operation.CompletedAt = nil
	}
	operation.NormalizedCode = firstNonEmpty(code, "manual_review_required")
	operation.LastError = message
}

func applyUnknown(intent *PaymentIntent, attempt *PaymentAttempt, operation *ProviderOperation, now time.Time, preservePending bool) {
	// Once the provider has acknowledged a pending operation, a later query
	// transport error must not erase that knowledge.
	if preservePending && operation.Status == OperationPending {
		scheduleRecovery(operation, now)
		return
	}
	operation.Status = OperationUnknown
	operation.SideEffectCertainty = SideEffectPossible
	operation.RetryMode = RetryQueryOrWait
	operation.CompletedAt = nil
	scheduleRecovery(operation, now)
	attempt.Status = AttemptUnknown
	attempt.RerouteDisposition = RerouteBlocked
	intent.Status = IntentProcessing
	intent.ActiveAttemptID = attempt.ID
}

func scheduleRecovery(operation *ProviderOperation, now time.Time) {
	operation.RetryCount++
	shift := operation.RetryCount
	if shift < 1 {
		shift = 1
	}
	if shift > 8 {
		shift = 8
	}
	delay := time.Duration(1<<shift) * time.Second
	next := now.UTC().Add(delay)
	operation.NextRetryAt = &next
}

func cloneOperation(operation ProviderOperation) ProviderOperation {
	clone := operation
	clone.RequestHash = append([]byte(nil), operation.RequestHash...)
	if operation.NextRetryAt != nil {
		next := *operation.NextRetryAt
		clone.NextRetryAt = &next
	}
	if operation.CompletedAt != nil {
		completed := *operation.CompletedAt
		clone.CompletedAt = &completed
	}
	return clone
}

func sameIntentState(left, right PaymentIntent) bool {
	left.Version, right.Version = 0, 0
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return left == right
}

func sameAttemptState(left, right PaymentAttempt) bool {
	left.Version, right.Version = 0, 0
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return left == right
}

func sameOperationState(left, right ProviderOperation) bool {
	left.Version, right.Version = 0, 0
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func isTerminalAttempt(status AttemptStatus) bool {
	switch status {
	case AttemptCaptured, AttemptDeclined, AttemptFailed:
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
