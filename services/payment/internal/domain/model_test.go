package domain

import (
	"errors"
	"testing"
	"time"
)

func TestSuccessfulAutomaticCaptureTransition(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatalf("mark sending: %v", err)
	}
	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   "simulator_a",
		OperationID:                operation.ID,
		ProviderIdempotencyKey:     operation.ProviderIdempotencyKey,
		Status:                     OutcomeSucceeded,
		SideEffectCertainty:        SideEffectConfirmed,
		NormalizedCode:             "captured",
		ProviderPaymentReference:   "psp_payment_success",
		ProviderOperationReference: "psp_operation_success",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("apply outcome: %v", err)
	}
	if !result.Changed || result.Conflict {
		t.Fatalf("unexpected apply result: %+v", result)
	}
	if intent.Status != IntentSucceeded || attempt.Status != AttemptCaptured || operation.Status != OperationSucceeded {
		t.Fatalf("unexpected states: intent=%s attempt=%s operation=%s", intent.Status, attempt.Status, operation.Status)
	}
	if intent.CapturedAmountMinor != intent.AmountMinor || intent.WinningAttemptID != attempt.ID || intent.ActiveAttemptID != "" {
		t.Fatalf("success facts were not recorded: %+v", intent)
	}
}

func TestUnknownBlocksNewConfirmationAndConverges(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:               operation.Provider,
		OperationID:            operation.ID,
		ProviderIdempotencyKey: operation.ProviderIdempotencyKey,
		Status:                 OutcomeUnknown,
		SideEffectCertainty:    SideEffectPossible,
		RetryMode:              RetryQueryOrWait,
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != OperationUnknown || attempt.Status != AttemptUnknown || intent.Status != IntentProcessing {
		t.Fatalf("unknown was collapsed: intent=%s attempt=%s operation=%s", intent.Status, attempt.Status, operation.Status)
	}

	secondAttempt := attempt
	secondAttempt.ID = "att_second"
	secondOperation := operation
	secondOperation.ID = "op_second"
	secondOperation.PaymentAttemptID = secondAttempt.ID
	if err := PrepareConfirmation(&intent, &secondAttempt, &secondOperation, now.Add(2*time.Second)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("UNKNOWN must block another attempt, got %v", err)
	}

	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		OperationID:                operation.ID,
		ProviderIdempotencyKey:     operation.ProviderIdempotencyKey,
		Status:                     OutcomeSucceeded,
		SideEffectCertainty:        SideEffectConfirmed,
		ProviderPaymentReference:   "psp_payment_after_unknown",
		ProviderOperationReference: "psp_operation_after_unknown",
	}, now.Add(3*time.Second))
	if err != nil || !result.Changed || intent.Status != IntentSucceeded {
		t.Fatalf("unknown did not converge to success: result=%+v err=%v intent=%+v", result, err, intent)
	}
}

func TestAmbiguousFailureIsDowngradedToUnknown(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:               operation.Provider,
		OperationID:            operation.ID,
		ProviderIdempotencyKey: operation.ProviderIdempotencyKey,
		Status:                 OutcomeFailed,
		SideEffectCertainty:    SideEffectPossible,
		NormalizedCode:         "http_500",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if operation.Status != OperationUnknown || intent.Status != IntentProcessing || attempt.RerouteDisposition != RerouteBlocked {
		t.Fatalf("ambiguous failure was treated as conclusive: %+v %+v %+v", intent, attempt, operation)
	}
}

func TestDeclineIsConclusiveButDoesNotAllowFallback(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:               operation.Provider,
		OperationID:            operation.ID,
		ProviderIdempotencyKey: operation.ProviderIdempotencyKey,
		Status:                 OutcomeDeclined,
		SideEffectCertainty:    SideEffectNone,
		NormalizedCode:         "do_not_honor",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if intent.Status != IntentFailed || attempt.Status != AttemptDeclined || attempt.RerouteDisposition != RerouteBlocked {
		t.Fatalf("unexpected decline state: %+v %+v", intent, attempt)
	}
}

func TestConflictingTerminalOutcomeRequiresReview(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		Status:                     OutcomeSucceeded,
		ProviderPaymentReference:   "psp_payment_conflict",
		ProviderOperationReference: "psp_operation_conflict",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider: operation.Provider,
		Status:   OutcomeDeclined,
	}, now.Add(2*time.Second))
	if !errors.Is(err, ErrConflictingOutcome) || !result.Conflict || intent.IntegrityStatus != IntegrityReviewRequired {
		t.Fatalf("conflict was not surfaced: result=%+v err=%v intent=%+v", result, err, intent)
	}
}

func TestReviewRequiredIsNotIgnoredAfterTerminalSuccess(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		Status:                     OutcomeSucceeded,
		ProviderPaymentReference:   "psp_payment_review",
		ProviderOperationReference: "psp_operation_review",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:       operation.Provider,
		Status:         OutcomeUnknown,
		RequiresReview: true,
	}, now.Add(2*time.Second))
	if !errors.Is(err, ErrConflictingOutcome) || !result.Conflict || !result.Changed {
		t.Fatalf("review_required conflict was ignored: result=%+v err=%v", result, err)
	}
	if intent.IntegrityStatus != IntegrityReviewRequired || intent.Status != IntentSucceeded {
		t.Fatalf("terminal funds state was regressed: %+v", intent)
	}
}

func TestReviewRequiredIsNotIgnoredAfterTerminalFailure(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:            operation.Provider,
		Status:              OutcomeDeclined,
		SideEffectCertainty: SideEffectNone,
		NormalizedCode:      "do_not_honor",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		Status:                     OutcomeUnknown,
		SideEffectCertainty:        SideEffectPossible,
		ProviderPaymentReference:   "late_authorization_ref",
		ProviderOperationReference: "late_authorization_ref",
		RequiresReview:             true,
	}, now.Add(2*time.Second))
	if !errors.Is(err, ErrConflictingOutcome) || !result.Conflict || !result.Changed {
		t.Fatalf("review_required after failure was ignored: result=%+v err=%v", result, err)
	}
	if intent.IntegrityStatus != IntegrityReviewRequired || intent.Status != IntentFailed || operation.Status != OperationFailed {
		t.Fatalf("failure fact or review state was lost: intent=%+v operation=%+v", intent, operation)
	}
}

func TestReviewRequiredUnknownAfterPendingStopsAutomaticRecovery(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:            operation.Provider,
		Status:              OutcomePending,
		SideEffectCertainty: SideEffectPossible,
		RetryMode:           RetryQueryOrWait,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:            operation.Provider,
		Status:              OutcomeUnknown,
		SideEffectCertainty: SideEffectPossible,
		RetryMode:           RetryQueryOrWait,
		RequiresReview:      true,
	}, now.Add(2*time.Second))
	if !errors.Is(err, ErrConflictingOutcome) || !result.Changed || !result.Conflict {
		t.Fatalf("review-required UNKNOWN was not persisted as a conflict: result=%+v err=%v", result, err)
	}
	if intent.IntegrityStatus != IntegrityReviewRequired {
		t.Fatalf("integrity escalation was lost: %+v", intent)
	}
	if intent.Status != IntentProcessing || attempt.Status != AttemptUnknown || operation.Status != OperationReviewRequired {
		t.Fatalf("review state is wrong: intent=%s attempt=%s operation=%s", intent.Status, attempt.Status, operation.Status)
	}
	if operation.RetryMode != RetryNone || operation.NextRetryAt != nil {
		t.Fatalf("review-required operation remained automatically recoverable: %+v", operation)
	}
}

func TestLateDuplicateSuccessWithStableReferencesIsNoop(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		Status:                     OutcomeSucceeded,
		ProviderPaymentReference:   "psp_payment_1",
		ProviderOperationReference: "psp_operation_1",
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		Status:                     OutcomeSucceeded,
		ProviderPaymentReference:   "psp_payment_1",
		ProviderOperationReference: "psp_operation_1",
	}, now.Add(2*time.Second))
	if err != nil || result.Changed || result.Conflict {
		t.Fatalf("late duplicate success was not a no-op: result=%+v err=%v", result, err)
	}
	if attempt.ProviderPaymentReference != "psp_payment_1" || operation.ProviderOperationReference != "psp_operation_1" {
		t.Fatalf("provider references were discarded: attempt=%+v operation=%+v", attempt, operation)
	}
}

func TestSuccessWithoutProviderReferenceBecomesUnknownAndRequiresReview(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}

	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:            operation.Provider,
		OperationID:         operation.ID,
		Status:              OutcomeSucceeded,
		SideEffectCertainty: SideEffectConfirmed,
	}, now.Add(time.Second))
	if !errors.Is(err, ErrConflictingOutcome) || !result.Conflict || !result.Changed || !result.IntentChanged {
		t.Fatalf("reference-less success was not escalated: result=%+v err=%v", result, err)
	}
	if intent.Status != IntentProcessing || intent.IntegrityStatus != IntegrityReviewRequired || intent.CapturedAmountMinor != 0 {
		t.Fatalf("reference-less success changed funds state: %+v", intent)
	}
	if attempt.Status != AttemptUnknown || operation.Status != OperationUnknown || operation.NextRetryAt == nil {
		t.Fatalf("reference-less success was not made recoverable: attempt=%+v operation=%+v", attempt, operation)
	}
}

func TestRepeatedUnknownOnlyAdvancesOperationRecoveryMetadata(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider: operation.Provider,
		Status:   OutcomeUnknown,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	intentVersion := intent.Version
	attemptVersion := attempt.Version
	operationVersion := operation.Version
	firstRetryAt := *operation.NextRetryAt

	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider: operation.Provider,
		Status:   OutcomeUnknown,
	}, now.Add(2*time.Second))
	if err != nil || !result.Changed || result.IntentChanged || result.Conflict {
		t.Fatalf("repeated UNKNOWN result=%+v err=%v", result, err)
	}
	if intent.Version != intentVersion || attempt.Version != attemptVersion {
		t.Fatalf("repeated UNKNOWN churned payment aggregate versions: intent=%d/%d attempt=%d/%d", intent.Version, intentVersion, attempt.Version, attemptVersion)
	}
	if operation.Version != operationVersion+1 || operation.NextRetryAt == nil || !operation.NextRetryAt.After(firstRetryAt) {
		t.Fatalf("recovery metadata did not advance: %+v", operation)
	}
}

func TestPendingThenTransportUnknownPreservesPendingAndAdvancesBackoff(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider: operation.Provider,
		Status:   OutcomePending,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	intentVersion := intent.Version
	attemptVersion := attempt.Version
	firstRetryAt := *operation.NextRetryAt
	repeatedPending, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider: operation.Provider,
		Status:   OutcomePending,
	}, now.Add(2*time.Second))
	if err != nil || !repeatedPending.Changed || repeatedPending.IntentChanged {
		t.Fatalf("repeated pending result=%+v err=%v", repeatedPending, err)
	}
	if intent.Version != intentVersion || attempt.Version != attemptVersion || operation.NextRetryAt == nil || !operation.NextRetryAt.After(firstRetryAt) {
		t.Fatalf("repeated pending churned aggregate or lost backoff: intent=%+v attempt=%+v operation=%+v", intent, attempt, operation)
	}
	secondRetryAt := *operation.NextRetryAt

	result, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider: operation.Provider,
		Status:   OutcomeUnknown,
	}, now.Add(3*time.Second))
	if err != nil || !result.Changed || result.IntentChanged {
		t.Fatalf("pending UNKNOWN result=%+v err=%v", result, err)
	}
	if operation.Status != OperationPending || attempt.Status != AttemptProcessing || intent.Status != IntentProcessing {
		t.Fatalf("pending state regressed: intent=%s attempt=%s operation=%s", intent.Status, attempt.Status, operation.Status)
	}
	if intent.Version != intentVersion || attempt.Version != attemptVersion || operation.NextRetryAt == nil || !operation.NextRetryAt.After(secondRetryAt) {
		t.Fatalf("pending recovery metadata is wrong: intent=%+v attempt=%+v operation=%+v", intent, attempt, operation)
	}
}

func TestManualReviewStopsAutomaticRecoveryAndPreservesFunds(t *testing.T) {
	intent, attempt, operation := preparedAggregate(t)
	now := time.Now().UTC()
	if err := MarkSending(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	result, err := RequireManualReview(
		&intent,
		&attempt,
		&operation,
		"provider_reference_collision",
		"provider reference belongs to another attempt",
		now.Add(time.Second),
	)
	if err != nil || !result.Changed || !result.IntentChanged || !result.Conflict {
		t.Fatalf("manual review result=%+v err=%v", result, err)
	}
	if intent.IntegrityStatus != IntegrityReviewRequired || intent.CapturedAmountMinor != 0 {
		t.Fatalf("manual review changed funds or missed integrity escalation: %+v", intent)
	}
	if attempt.Status != AttemptUnknown || operation.Status != OperationReviewRequired || operation.RetryMode != RetryNone || operation.NextRetryAt != nil {
		t.Fatalf("manual review remained automatically recoverable: attempt=%+v operation=%+v", attempt, operation)
	}

	late, err := ApplyProviderOutcome(&intent, &attempt, &operation, ProviderOutcome{
		Provider:                   operation.Provider,
		Status:                     OutcomeSucceeded,
		ProviderPaymentReference:   "another_reference",
		ProviderOperationReference: "another_reference",
	}, now.Add(2*time.Second))
	if !errors.Is(err, ErrConflictingOutcome) || !late.Conflict || intent.CapturedAmountMinor != 0 || operation.Status != OperationReviewRequired {
		t.Fatalf("automatic outcome escaped manual review: result=%+v err=%v intent=%+v operation=%+v", late, err, intent, operation)
	}
}

func preparedAggregate(t *testing.T) (PaymentIntent, PaymentAttempt, ProviderOperation) {
	t.Helper()
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	intent, err := NewIntent("pi_test", "merchant_test", "order_1", 2599, "USD", "tok_test", now)
	if err != nil {
		t.Fatal(err)
	}
	attempt := PaymentAttempt{
		ID:                "att_test",
		PaymentIntentID:   intent.ID,
		AttemptNumber:     1,
		RoutingDecisionID: "rtd_test",
		Provider:          "simulator_a",
		CreatedAt:         now,
	}
	operation := ProviderOperation{
		ID:                     "op_test",
		PaymentIntentID:        intent.ID,
		PaymentAttemptID:       attempt.ID,
		Kind:                   OperationSale,
		AmountMinor:            intent.AmountMinor,
		Currency:               intent.Currency,
		Provider:               attempt.Provider,
		ProviderIdempotencyKey: "op_test",
		MerchantReference:      "op_test",
		RequestHash:            []byte("request hash"),
		CreatedAt:              now,
	}
	if err := PrepareConfirmation(&intent, &attempt, &operation, now); err != nil {
		t.Fatal(err)
	}
	return intent, attempt, operation
}
