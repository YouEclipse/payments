//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProviderConnectionMigrationUpgradesLegacyOutcomePayloads(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)

	schema := fmt.Sprintf("payment_upgrade_%d", time.Now().UnixNano())
	schemaIdentifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schemaIdentifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+schemaIdentifier+" CASCADE"); err != nil {
			t.Errorf("drop upgrade-test schema: %v", err)
		}
	})

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["standard_conforming_strings"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	applyLegacyPaymentMigrations(t, ctx, pool, "001_payment.sql", "002_payment_safety.sql")
	seedLegacyPaymentOutcome(t, ctx, pool)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("upgrade payment schema: %v", err)
	}
	var createdAt, recoveryDeadline time.Time
	var firstDispatched *time.Time
	var budget int
	var policy string
	if err := pool.QueryRow(ctx, `
		SELECT created_at,first_dispatched_at,recovery_deadline_at,recovery_budget_remaining,recovery_policy_version
		FROM provider_operations WHERE id='op_legacy_payment'`,
	).Scan(&createdAt, &firstDispatched, &recoveryDeadline, &budget, &policy); err != nil {
		t.Fatal(err)
	}
	if firstDispatched != nil || !recoveryDeadline.Equal(createdAt.Add(24*time.Hour)) ||
		budget != 32 || policy != "local_sale_recovery.v1" {
		t.Fatalf("legacy recovery was reset or fabricated: first=%v created=%s deadline=%s budget=%d policy=%q",
			firstDispatched, createdAt, recoveryDeadline, budget, policy)
	}

	for _, table := range []string{"provider_outcome_inbox", "provider_outcome_quarantine"} {
		var relationalConnection, relationalRevision, relationalNamespace string
		var payloadConnection, payloadRevision, payloadNamespace string
		query := fmt.Sprintf(`
			SELECT provider_connection_id,provider_connection_revision,provider_namespace_id,
			       sanitized_payload ->> 'provider_connection_id',
			       sanitized_payload ->> 'provider_connection_revision',
			       sanitized_payload ->> 'provider_namespace_id'
			FROM %s WHERE event_id=$1`, table)
		eventID := "evt_legacy_payment"
		if table == "provider_outcome_quarantine" {
			eventID = "evt_legacy_quarantine"
		}
		if err := pool.QueryRow(ctx, query, eventID).Scan(
			&relationalConnection, &relationalRevision, &relationalNamespace,
			&payloadConnection, &payloadRevision, &payloadNamespace,
		); err != nil {
			t.Fatalf("read upgraded %s: %v", table, err)
		}
		if relationalConnection != "simulator_a" || relationalRevision != "legacy_v1" || relationalNamespace != "simulator_a" ||
			payloadConnection != relationalConnection || payloadRevision != relationalRevision || payloadNamespace != relationalNamespace {
			t.Fatalf("%s identity relational=(%q,%q,%q) payload=(%q,%q,%q)", table,
				relationalConnection, relationalRevision, relationalNamespace,
				payloadConnection, payloadRevision, payloadNamespace)
		}
	}

	duplicatePayload := []byte(`{
		"event_id":"evt_legacy_payment",
		"operation_id":"op_legacy_payment",
		"provider_connection_id":"simulator_a",
		"provider_connection_revision":"legacy_v1",
		"provider_namespace_id":"simulator_a",
		"provider":"simulator_a",
		"provider_idempotency_key":"op_legacy_payment",
		"status":"unknown",
		"side_effect_certainty":"possible"
	}`)
	result, err := NewStore(pool).ApplyOutcome(ctx, domain.ProviderOutcome{
		EventID:                    "evt_legacy_payment",
		OperationID:                "op_legacy_payment",
		ProviderConnectionID:       "simulator_a",
		ProviderConnectionRevision: "legacy_v1",
		ProviderNamespaceID:        "simulator_a",
		Provider:                   "simulator_a",
		ProviderIdempotencyKey:     "op_legacy_payment",
		Status:                     domain.OutcomeUnknown,
		SideEffectCertainty:        domain.SideEffectPossible,
	}, duplicatePayload, time.Now().UTC(), nil)
	if err != nil || !result.Duplicate || result.Rejected {
		t.Fatalf("upgraded duplicate result=%+v err=%v", result, err)
	}
	var falseConflicts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_outcome_quarantine
		WHERE provider_namespace_id='simulator_a' AND event_id='evt_legacy_payment'`,
	).Scan(&falseConflicts); err != nil {
		t.Fatal(err)
	}
	if falseConflicts != 0 {
		t.Fatalf("upgraded duplicate created %d false quarantine records", falseConflicts)
	}
}

func TestRoutingExecutableUntilMigrationPreservesLegacyHistoryAndRejectsUnsafeFutureWrites(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)

	schema := fmt.Sprintf("payment_routing_upgrade_%d", time.Now().UnixNano())
	schemaIdentifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schemaIdentifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+schemaIdentifier+" CASCADE"); err != nil {
			t.Errorf("drop routing-upgrade-test schema: %v", err)
		}
	})

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["standard_conforming_strings"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	applyLegacyPaymentMigrations(t, ctx, pool,
		"001_payment.sql", "002_payment_safety.sql",
		"003_provider_connection_identity.sql", "004_routing_context_snapshot.sql",
	)

	// Router canonicalizes a 123456789ns source artifact downward before the
	// Payment boundary. The upgrade/round-trip fixture uses that exact
	// PostgreSQL-safe authority instant: 123456000ns, never a rounded-up value.
	seedAt := time.Now().UTC().Truncate(time.Second).Add(123456 * time.Microsecond)
	legacyIntent, legacyAttempt := routingEvidenceFixture(t, "upgrade_legacy", seedAt)
	seedRoutingUpgradeAggregate(t, ctx, pool, legacyIntent, legacyAttempt, "", "")

	oldIntent, oldAttempt := routingEvidenceFixture(t, "upgrade_old_v1", seedAt)
	oldDecisionJSON, oldDecisionID := oldRoutingDecisionWithoutExecutableUntil(t, oldAttempt.RoutingDecisionSnapshot)
	oldAttempt.RoutingDecisionID = oldDecisionID
	assertOldRoutingEvidenceIsOtherwiseValid(t, oldIntent, oldAttempt, oldDecisionJSON, seedAt)
	seedRoutingUpgradeAggregate(t, ctx, pool, oldIntent, oldAttempt, oldAttempt.RoutingContextSnapshot, oldDecisionJSON)

	boundaryIntent, boundaryAttempt := routingEvidenceFixture(t, "upgrade_004_boundary", seedAt)
	boundaryDecisionJSON := removeJSONMember(t, boundaryAttempt.RoutingDecisionSnapshot, "candidate_order")
	seedRoutingUpgradeAggregate(t, ctx, pool, boundaryIntent, boundaryAttempt,
		boundaryAttempt.RoutingContextSnapshot, boundaryDecisionJSON)

	// Migration 004 deliberately did not constrain candidate_order. The insert
	// above proves this historical row was legal at the old schema boundary;
	// migration 006 must detect it before installing its stronger constraint.
	var preflightViolations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM payment_attempts
		WHERE id=$1 AND NOT (routing_decision_snapshot ? 'candidate_order')`, boundaryAttempt.ID,
	).Scan(&preflightViolations); err != nil {
		t.Fatal(err)
	}
	if preflightViolations != 1 {
		t.Fatalf("004 boundary fixture did not expose the expected 006 violation: %d", preflightViolations)
	}

	migrationErr := Migrate(ctx, pool)
	var constraintErr *pgconn.PgError
	if !errors.As(migrationErr, &constraintErr) || constraintErr.Code != "23514" {
		t.Fatalf("006 accepted a 004-only routing snapshot: %v", migrationErr)
	}
	var applied005, applied006, deadlineColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_schema_migrations WHERE version='005_payment_recovery.sql'`).Scan(&applied005); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_schema_migrations WHERE version='006_risk_routing_evidence.sql'`).Scan(&applied006); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='payment_attempts'
		  AND column_name='routing_executable_until'`,
	).Scan(&deadlineColumns); err != nil {
		t.Fatal(err)
	}
	if applied005 != 1 || applied006 != 0 || deadlineColumns != 0 {
		t.Fatalf("failed 006 did not roll back atomically: migration005=%d migration006=%d deadline_columns=%d",
			applied005, applied006, deadlineColumns)
	}

	// This is an explicit operator repair of data that was legal under 004.
	// Disabling only the immutable-evidence trigger is deliberate and scoped to
	// the isolated upgrade schema; the restored decision is the original typed
	// Router fixture, including candidate_order.
	if _, err := pool.Exec(ctx, `ALTER TABLE payment_attempts DISABLE TRIGGER payment_attempt_routing_snapshot_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE payment_attempts SET routing_decision_snapshot=$2::jsonb WHERE id=$1`,
		boundaryAttempt.ID, boundaryAttempt.RoutingDecisionSnapshot,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE payment_attempts ENABLE TRIGGER payment_attempt_routing_snapshot_immutable`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("upgrade after explicit historical-data repair: %v", err)
	}

	var legacySnapshotsAbsent, legacyDeadlineAbsent bool
	if err := pool.QueryRow(ctx, `
		SELECT routing_context_snapshot IS NULL AND routing_decision_snapshot IS NULL,
		       routing_executable_until IS NULL
		FROM payment_attempts WHERE id=$1`, legacyAttempt.ID,
	).Scan(&legacySnapshotsAbsent, &legacyDeadlineAbsent); err != nil {
		t.Fatal(err)
	}
	if !legacySnapshotsAbsent || !legacyDeadlineAbsent {
		t.Fatalf("upgrade fabricated routing authority for snapshot-free legacy row: snapshots_absent=%v deadline_absent=%v",
			legacySnapshotsAbsent, legacyDeadlineAbsent)
	}

	var oldDeadlineAbsent, oldJSONDeadlineAbsent bool
	if err := pool.QueryRow(ctx, `
		SELECT routing_executable_until IS NULL,
		       NOT (routing_decision_snapshot ? 'executable_until')
		FROM payment_attempts WHERE id=$1`, oldAttempt.ID,
	).Scan(&oldDeadlineAbsent, &oldJSONDeadlineAbsent); err != nil {
		t.Fatal(err)
	}
	if !oldDeadlineAbsent || !oldJSONDeadlineAbsent {
		t.Fatalf("upgrade invented a deadline for old v1 evidence: column_absent=%v json_absent=%v",
			oldDeadlineAbsent, oldJSONDeadlineAbsent)
	}

	store := NewStore(pool)
	if _, err := store.MarkOperationSending(ctx, "op_"+oldAttempt.ID, seedAt.Add(2*time.Second)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("old v1 snapshot without routing authority reached first dispatch: %v", err)
	}
	var oldOperationStatus, oldStopReason string
	var oldFirstDispatchAbsent, oldDeadlineStillAbsent bool
	if err := pool.QueryRow(ctx, `
		SELECT o.status,o.first_dispatched_at IS NULL,COALESCE(o.recovery_stop_reason,''),
		       a.routing_executable_until IS NULL
		FROM provider_operations o JOIN payment_attempts a ON a.id=o.payment_attempt_id
		WHERE o.id=$1`, "op_"+oldAttempt.ID,
	).Scan(&oldOperationStatus, &oldFirstDispatchAbsent, &oldStopReason, &oldDeadlineStillAbsent); err != nil {
		t.Fatal(err)
	}
	if oldOperationStatus != string(domain.OperationReviewRequired) || !oldFirstDispatchAbsent ||
		oldStopReason != "routing_authority_expired_before_first_dispatch" || !oldDeadlineStillAbsent {
		t.Fatalf("old v1 first dispatch did not fail closed: status=%q first_absent=%v reason=%q deadline_absent=%v",
			oldOperationStatus, oldFirstDispatchAbsent, oldStopReason, oldDeadlineStillAbsent)
	}

	legacySending, err := store.MarkOperationSending(ctx, "op_"+legacyAttempt.ID, seedAt.Add(3*time.Second))
	if err != nil {
		t.Fatalf("snapshot-free legacy aggregate stopped working after upgrade: %v", err)
	}
	if legacySending.Operation.Status != domain.OperationSending || legacySending.Operation.FirstDispatchedAt == nil ||
		legacySending.Attempt.RoutingExecutableUntil != nil {
		t.Fatalf("snapshot-free legacy dispatch changed semantics: %+v", legacySending)
	}

	newIntent, newAttempt := routingEvidenceFixture(t, "upgrade_new_write", seedAt)
	seedRoutingUpgradeIntent(t, ctx, pool, newIntent)
	var decision clients.RoutingDecision
	if err := json.Unmarshal([]byte(newAttempt.RoutingDecisionSnapshot), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.ExecutableUntil.IsZero() {
		t.Fatal("new routing fixture omitted executable_until")
	}
	for _, format := range []struct {
		name     string
		text     string
		deadline time.Time
	}{
		{name: "whole second", text: "2026-08-27T01:02:03Z", deadline: time.Date(2026, 8, 27, 1, 2, 3, 0, time.UTC)},
		{name: "one fractional digit", text: "2026-08-27T01:02:03.1Z", deadline: time.Date(2026, 8, 27, 1, 2, 3, 100000000, time.UTC)},
		{name: "six fractional digits", text: "2026-08-27T01:02:03.123456Z", deadline: time.Date(2026, 8, 27, 1, 2, 3, 123456000, time.UTC)},
	} {
		t.Run("canonical SQL format/"+format.name, func(t *testing.T) {
			decisionJSON := replaceJSONMember(t, newAttempt.RoutingDecisionSnapshot, "executable_until", format.text)
			if err := insertRoutingUpgradeAttempt(ctx, pool, newAttempt, decisionJSON, &format.deadline); err != nil {
				t.Fatalf("canonical deadline rejected: %v", err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM payment_attempts WHERE id=$1`, newAttempt.ID); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, test := range []struct {
		name         string
		decisionJSON string
		deadline     *time.Time
	}{
		{name: "missing relational deadline", decisionJSON: newAttempt.RoutingDecisionSnapshot},
		{name: "unparseable JSON deadline", decisionJSON: replaceJSONMember(t, newAttempt.RoutingDecisionSnapshot, "executable_until", "not-a-timestamp"), deadline: timePointer(decision.ExecutableUntil)},
		{name: "seven fractional digits", decisionJSON: replaceJSONMember(t, newAttempt.RoutingDecisionSnapshot, "executable_until", "2026-08-27T01:02:03.1234560Z"), deadline: timePointer(time.Date(2026, 8, 27, 1, 2, 3, 123456000, time.UTC))},
		{name: "nine fractional digits", decisionJSON: replaceJSONMember(t, newAttempt.RoutingDecisionSnapshot, "executable_until", "2026-08-27T01:02:03.123456000Z"), deadline: timePointer(time.Date(2026, 8, 27, 1, 2, 3, 123456000, time.UTC))},
		{name: "numeric UTC offset", decisionJSON: replaceJSONMember(t, newAttempt.RoutingDecisionSnapshot, "executable_until", "2026-08-27T01:02:03.123456+00:00"), deadline: timePointer(time.Date(2026, 8, 27, 1, 2, 3, 123456000, time.UTC))},
		{name: "JSON and relational deadline differ", decisionJSON: newAttempt.RoutingDecisionSnapshot, deadline: timePointer(decision.ExecutableUntil.Add(time.Second))},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := insertRoutingUpgradeAttempt(ctx, pool, newAttempt, test.decisionJSON, test.deadline)
			var checkErr *pgconn.PgError
			if !errors.As(err, &checkErr) || checkErr.Code != "23514" {
				t.Fatalf("unsafe versioned insert was accepted: %v", err)
			}
		})
	}
	deadline := decision.ExecutableUntil.UTC()
	if err := insertRoutingUpgradeAttempt(ctx, pool, newAttempt, newAttempt.RoutingDecisionSnapshot, &deadline); err != nil {
		t.Fatalf("valid JSON/column routing authority pair was rejected: %v", err)
	}
	var storedDeadline time.Time
	var storedJSONDeadline string
	if err := pool.QueryRow(ctx, `
		SELECT routing_executable_until,routing_decision_snapshot->>'executable_until'
		FROM payment_attempts WHERE id=$1`, newAttempt.ID,
	).Scan(&storedDeadline, &storedJSONDeadline); err != nil {
		t.Fatal(err)
	}
	parsedJSONDeadline, err := time.Parse(time.RFC3339Nano, storedJSONDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if !storedDeadline.Equal(deadline) || !parsedJSONDeadline.Equal(deadline) || storedDeadline.Nanosecond() != 123456000 {
		t.Fatalf("stored routing authority changed: column=%s JSON=%s want=%s", storedDeadline, storedJSONDeadline, deadline)
	}
}

func oldRoutingDecisionWithoutExecutableUntil(t *testing.T, source string) (string, string) {
	t.Helper()
	var decision clients.RoutingDecision
	if err := json.Unmarshal([]byte(source), &decision); err != nil {
		t.Fatal(err)
	}
	decision.ExecutableUntil = time.Time{}
	decision.DecisionID = clients.VersionedDecisionContentID(decision)
	payload, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return removeJSONMember(t, string(payload), "executable_until"), decision.DecisionID
}

func assertOldRoutingEvidenceIsOtherwiseValid(
	t *testing.T,
	intent domain.PaymentIntent,
	attempt domain.PaymentAttempt,
	decisionJSON string,
	now time.Time,
) {
	t.Helper()
	var snapshot routingcontext.Context
	if err := json.Unmarshal([]byte(attempt.RoutingContextSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ValidateOnline(now, routingcontext.OnlineMaxAge); err != nil {
		t.Fatalf("old routing context is not a valid online v1 snapshot: %v", err)
	}
	digest, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var decision clients.RoutingDecision
	if err := json.Unmarshal([]byte(decisionJSON), &decision); err != nil {
		t.Fatal(err)
	}
	selected := decision.SelectedCandidate
	if !decision.ExecutableUntil.IsZero() || !clients.ValidVersionedDecisionID(decision) ||
		decision.ContextSchemaVersion != routingcontext.SchemaVersion || decision.ContextDigest != digest ||
		decision.DecisionID != attempt.RoutingDecisionID || snapshot.Base.PaymentID != intent.ID ||
		selected.CandidateID != attempt.ProviderConnectionID ||
		selected.ConnectionRevision != attempt.ProviderConnectionRevision ||
		selected.ProviderNamespaceID != attempt.ProviderNamespaceID || selected.Provider != attempt.Provider ||
		len(decision.CandidateOrder) == 0 || decision.CandidateOrder[0] != selected.CandidateID {
		t.Fatalf("old v1 fixture is invalid for a reason other than its absent executable_until: %+v", decision)
	}
}

func seedRoutingUpgradeAggregate(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	intent domain.PaymentIntent,
	attempt domain.PaymentAttempt,
	contextJSON string,
	decisionJSON string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_intents (
			id,merchant_id,merchant_reference,amount_minor,currency,payment_method_type,
			payment_method_token,capture_method,status,integrity_status,version,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'processing','healthy',1,$9,$9)`,
		intent.ID, intent.MerchantID, intent.MerchantReference, intent.AmountMinor, intent.Currency,
		intent.PaymentMethodType, intent.PaymentMethodToken, intent.CaptureMethod, intent.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_attempts (
			id,payment_intent_id,attempt_number,routing_decision_id,
			provider_connection_id,provider_connection_revision,provider_namespace_id,provider,
			status,reroute_disposition,version,created_at,updated_at,
			routing_context_snapshot,routing_decision_snapshot
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'created','blocked',0,$9,$9,
		          NULLIF($10,'')::jsonb,NULLIF($11,'')::jsonb)`,
		attempt.ID, intent.ID, attempt.AttemptNumber, attempt.RoutingDecisionID,
		attempt.ProviderConnectionID, attempt.ProviderConnectionRevision, attempt.ProviderNamespaceID,
		attempt.Provider, attempt.CreatedAt, contextJSON, decisionJSON,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE payment_intents SET active_attempt_id=$2,latest_attempt_id=$2 WHERE id=$1`,
		intent.ID, attempt.ID,
	); err != nil {
		t.Fatal(err)
	}
	operationID := "op_" + attempt.ID
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_operations (
			id,payment_intent_id,payment_attempt_id,kind,amount_minor,currency,
			provider_connection_id,provider_connection_revision,provider_namespace_id,provider,
			provider_idempotency_key,merchant_reference,request_hash,status,
			side_effect_certainty,retry_mode,retry_count,version,created_at,updated_at
		) VALUES ($1,$2,$3,'sale',$4,$5,$6,$7,$8,$9,$1,$1,$10,'created',
		          'none','same_key',0,0,$11,$11)`,
		operationID, intent.ID, attempt.ID, intent.AmountMinor, intent.Currency,
		attempt.ProviderConnectionID, attempt.ProviderConnectionRevision, attempt.ProviderNamespaceID,
		attempt.Provider, []byte("request_"+operationID), attempt.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
}

func seedRoutingUpgradeIntent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, intent domain.PaymentIntent) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_intents (
			id,merchant_id,merchant_reference,amount_minor,currency,payment_method_type,
			payment_method_token,capture_method,status,integrity_status,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'requires_confirmation','healthy',$9,$9)`,
		intent.ID, intent.MerchantID, intent.MerchantReference, intent.AmountMinor, intent.Currency,
		intent.PaymentMethodType, intent.PaymentMethodToken, intent.CaptureMethod, intent.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
}

func insertRoutingUpgradeAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	attempt domain.PaymentAttempt,
	decisionJSON string,
	deadline *time.Time,
) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO payment_attempts (
			id,payment_intent_id,attempt_number,routing_decision_id,
			provider_connection_id,provider_connection_revision,provider_namespace_id,provider,
			status,reroute_disposition,version,created_at,updated_at,
			routing_context_snapshot,routing_decision_snapshot,routing_executable_until
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'created','blocked',0,$9,$9,$10::jsonb,$11::jsonb,$12)`,
		attempt.ID, attempt.PaymentIntentID, attempt.AttemptNumber, attempt.RoutingDecisionID,
		attempt.ProviderConnectionID, attempt.ProviderConnectionRevision, attempt.ProviderNamespaceID,
		attempt.Provider, attempt.CreatedAt, attempt.RoutingContextSnapshot, decisionJSON, deadline,
	)
	return err
}

func removeJSONMember(t *testing.T, source, member string) string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(source), &object); err != nil {
		t.Fatal(err)
	}
	delete(object, member)
	payload, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func replaceJSONMember(t *testing.T, source, member string, value any) string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(source), &object); err != nil {
		t.Fatal(err)
	}
	object[member] = value
	payload, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func applyLegacyPaymentMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versions ...string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE payment_schema_migrations (
			version text PRIMARY KEY,
			checksum bytea,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatal(err)
	}
	for _, version := range versions {
		body, err := fs.ReadFile(migrationFiles, "migrations/"+version)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply legacy migration %s: %v", version, err)
		}
		checksum := sha256.Sum256(body)
		if _, err := tx.Exec(ctx,
			`INSERT INTO payment_schema_migrations(version,checksum) VALUES($1,$2)`,
			version, checksum[:],
		); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("record legacy migration %s: %v", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit legacy migration %s: %v", version, err)
		}
	}
}

func seedLegacyPaymentOutcome(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_intents (
			id,merchant_id,merchant_reference,amount_minor,currency,payment_method_type,
			payment_method_token,capture_method,status,created_at,updated_at
		) VALUES ('pi_legacy_payment','merchant_legacy','order_legacy',100,'USD','card',
		          'pm_legacy','automatic','processing',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_attempts (
			id,payment_intent_id,attempt_number,routing_decision_id,provider,status,
			reroute_disposition,created_at,updated_at
		) VALUES ('att_legacy_payment','pi_legacy_payment',1,'rtd_legacy_payment',
		          'simulator_a','unknown','blocked',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_operations (
			id,payment_intent_id,payment_attempt_id,kind,amount_minor,currency,provider,
			provider_idempotency_key,merchant_reference,request_hash,status,
			side_effect_certainty,retry_mode,created_at,updated_at
		) VALUES ('op_legacy_payment','pi_legacy_payment','att_legacy_payment','sale',100,'USD',
		          'simulator_a','op_legacy_payment','op_legacy_payment',$1,'unknown',
		          'possible','query_or_wait',$2,$2)`, []byte("legacy_hash"), now); err != nil {
		t.Fatal(err)
	}
	legacyPayload, err := json.Marshal(map[string]any{
		"event_id":                 "evt_legacy_payment",
		"operation_id":             "op_legacy_payment",
		"provider":                 "simulator_a",
		"provider_idempotency_key": "op_legacy_payment",
		"status":                   "unknown",
		"side_effect_certainty":    "possible",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_outcome_inbox (
			provider,event_id,operation_id,provider_idempotency_key,status,
			side_effect_certainty,received_at,processed_at,processing_status,sanitized_payload
		) VALUES ('simulator_a','evt_legacy_payment','op_legacy_payment','op_legacy_payment',
		          'unknown','possible',$1,$1,'processed',$2)`, now, legacyPayload); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_outcome_quarantine (
			provider,event_id,operation_id,provider_idempotency_key,reason,sanitized_payload,received_at
		) VALUES ('simulator_a','evt_legacy_quarantine','op_legacy_payment','op_legacy_payment',
		          'legacy fixture',$1,$2)`, []byte(`{"event_id":"evt_legacy_quarantine","provider":"simulator_a"}`), now); err != nil {
		t.Fatal(err)
	}
}
