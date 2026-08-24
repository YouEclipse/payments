//go:build integration

package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresStoreOperationAndWebhookOutbox(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var checksumLength int
	if err := store.pool.QueryRow(ctx, `
		SELECT octet_length(checksum)
		FROM provider_gateway.schema_migrations
		WHERE version='0001_init.sql'`).Scan(&checksumLength); err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	if checksumLength != sha256.Size {
		t.Fatalf("migration checksum length=%d, want %d", checksumLength, sha256.Size)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	now := time.Now().UTC()
	op := Operation{
		OperationID: "op_it_" + suffix, AttemptID: "att_it_" + suffix, Provider: "provider_it",
		ProviderIdempotencyKey: "op_it_" + suffix, RequestHash: "hash_it", Status: StatusCreated,
		SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
	}
	created, inserted, err := store.CreateOrGet(ctx, op)
	if err != nil || !inserted || created.OperationID != op.OperationID {
		t.Fatalf("create operation: inserted=%v operation=%+v err=%v", inserted, created, err)
	}
	replayed, inserted, err := store.CreateOrGet(ctx, op)
	if err != nil || inserted || replayed.OperationID != op.OperationID {
		t.Fatalf("replay operation: inserted=%v operation=%+v err=%v", inserted, replayed, err)
	}

	wrongProviderOutcome := ProviderOutcome{
		EventID: "evt_wrong_provider_" + suffix, OperationID: op.OperationID, Provider: "another_provider",
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "wrong_provider_ref", OccurredAt: now,
	}
	if _, inserted, err := store.ApplyWebhook(ctx, wrongProviderOutcome); !errors.Is(err, ErrNotFound) || inserted {
		t.Fatalf("cross-provider webhook: inserted=%v err=%v, want ErrNotFound", inserted, err)
	}
	unchanged, err := store.Get(ctx, op.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != StatusCreated || unchanged.ProviderReference != "" {
		t.Fatalf("cross-provider webhook mutated operation: %+v", unchanged)
	}

	outcome := ProviderOutcome{
		EventID: "evt_it_" + suffix, OperationID: op.OperationID, Provider: op.Provider,
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_it", OccurredAt: now,
	}
	storedOutcome, inserted, err := store.ApplyWebhook(ctx, outcome)
	if err != nil || !inserted || storedOutcome.Status != StatusSucceeded {
		t.Fatalf("apply webhook: inserted=%v outcome=%+v err=%v", inserted, storedOutcome, err)
	}
	_, inserted, err = store.ApplyWebhook(ctx, outcome)
	if err != nil || inserted {
		t.Fatalf("duplicate webhook: inserted=%v err=%v", inserted, err)
	}
	items, err := store.PendingOutbox(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.Outcome.EventID == outcome.EventID {
			found = true
			if err := store.MarkOutboxDelivered(ctx, item.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("webhook outcome was not written to the durable outbox")
	}
}

func TestPostgresStoreWebhookCorrelationAndMonotonicity(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	now := time.Now().UTC()
	op := Operation{
		OperationID: "op_monotonic_" + suffix, AttemptID: "att_monotonic_" + suffix, Provider: "provider_it",
		ProviderIdempotencyKey: "op_monotonic_" + suffix, RequestHash: "hash_it", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_a", CreatedAt: now, UpdatedAt: now,
	}
	if _, inserted, err := store.CreateOrGet(ctx, op); err != nil || !inserted {
		t.Fatalf("create operation: inserted=%v err=%v", inserted, err)
	}

	lateAuthorization := ProviderOutcome{
		EventID: "evt_late_auth_" + suffix, OperationID: op.OperationID, Provider: op.Provider,
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "provider_ref_a", OccurredAt: now,
	}
	stored, inserted, err := store.ApplyWebhook(ctx, lateAuthorization)
	if err != nil || !inserted {
		t.Fatalf("late authorization: inserted=%v err=%v", inserted, err)
	}
	if stored.Status != StatusSucceeded || stored.SideEffectCertainty != "confirmed" {
		t.Fatalf("late authorization downgraded capture: %+v", stored)
	}

	conflictingDuplicate := lateAuthorization
	conflictingDuplicate.OperationID = "another_operation"
	if _, inserted, err := store.ApplyWebhook(ctx, conflictingDuplicate); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("reused event: inserted=%v err=%v, want ErrWebhookEventConflict", inserted, err)
	}

	referenceConflict := ProviderOutcome{
		EventID: "evt_ref_conflict_" + suffix, OperationID: op.OperationID, Provider: op.Provider,
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_b", OccurredAt: now,
	}
	stored, inserted, err = store.ApplyWebhook(ctx, referenceConflict)
	if err != nil || !inserted {
		t.Fatalf("reference conflict: inserted=%v err=%v", inserted, err)
	}
	if stored.Status != StatusReview || stored.ProviderReference != "provider_ref_a" {
		t.Fatalf("reference conflict was not quarantined: %+v", stored)
	}
}

func TestPostgresStoreAuthorizationAndFailureConflictInEitherOrder(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	suffix := fmt.Sprintf("%d", now.UnixNano())
	seed := func(id, status, certainty string) {
		t.Helper()
		_, inserted, err := store.CreateOrGet(ctx, Operation{
			OperationID: id, AttemptID: "att_" + id, Provider: "provider_it", ProviderIdempotencyKey: id,
			RequestHash: "hash_" + id, Status: status, SideEffectCertainty: certainty, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil || !inserted {
			t.Fatalf("seed %s: inserted=%v err=%v", id, inserted, err)
		}
	}

	declineFirstID := "op_decline_auth_" + suffix
	seed(declineFirstID, StatusFailed, "none")
	lateAuthorization, inserted, err := store.ApplyWebhook(ctx, ProviderOutcome{
		EventID: "evt_decline_auth_" + suffix, OperationID: declineFirstID, Provider: "provider_it",
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "auth_ref_" + suffix,
		FailureCode: "authorization_only", OccurredAt: now,
	})
	if err != nil || !inserted || lateAuthorization.Status != StatusReview || lateAuthorization.FailureCode != "authorization_failure_conflict" {
		t.Fatalf("decline then authorization: inserted=%v outcome=%+v err=%v", inserted, lateAuthorization, err)
	}

	authFirstID := "op_auth_decline_" + suffix
	seed(authFirstID, StatusSending, "possible")
	authorization, inserted, err := store.ApplyWebhook(ctx, ProviderOutcome{
		EventID: "evt_auth_first_" + suffix, OperationID: authFirstID, Provider: "provider_it",
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "auth_first_ref_" + suffix,
		FailureCode: "authorization_only", OccurredAt: now,
	})
	if err != nil || !inserted || authorization.Status != StatusUnknown {
		t.Fatalf("authorization first: inserted=%v outcome=%+v err=%v", inserted, authorization, err)
	}
	decline, inserted, err := store.ApplyWebhook(ctx, ProviderOutcome{
		EventID: "evt_decline_second_" + suffix, OperationID: authFirstID, Provider: "provider_it",
		Status: StatusFailed, SideEffectCertainty: "none", ProviderReference: authorization.ProviderReference,
		FailureCode: "do_not_honor", OccurredAt: now.Add(time.Second),
	})
	if err != nil || !inserted || decline.Status != StatusReview || decline.FailureCode != "authorization_failure_conflict" {
		t.Fatalf("authorization then decline: inserted=%v outcome=%+v err=%v", inserted, decline, err)
	}
}
