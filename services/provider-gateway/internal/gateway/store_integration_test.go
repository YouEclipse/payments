//go:build integration

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGatewayMigrationsCreateFreshIdentitySchema(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	schema := fmt.Sprintf("provider_gateway_fresh_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	for version, migration := range []string{initialMigration, providerConnectionMigration} {
		if _, err := pool.Exec(ctx, strings.ReplaceAll(migration, "provider_gateway", schema)); err != nil {
			t.Fatalf("apply isolated fresh migration %d: %v", version+1, err)
		}
	}

	now := time.Now().UTC()
	insertOperation := fmt.Sprintf(`
		INSERT INTO %s.operations
		(operation_id,attempt_id,provider_connection_id,provider_connection_revision,provider_namespace_id,
		 provider,provider_idempotency_key,request_hash,request_hash_version,status,side_effect_certainty,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,2,'created','none',$9,$9)`, schema)
	if _, err := pool.Exec(ctx, insertOperation,
		"op_fresh_primary", "att_fresh_primary", "provider_a_primary", "provider_a_primary_v1",
		"provider_a", "provider_a", "provider_key_shared", "hash_primary", now,
	); err != nil {
		t.Fatalf("insert fresh current operation: %v", err)
	}
	if _, err := pool.Exec(ctx, insertOperation,
		"op_fresh_alias", "att_fresh_alias", "provider_a", legacyConnectionRevision,
		"provider_a", "provider_a", "provider_key_shared", "hash_alias", now,
	); err == nil {
		t.Fatal("same namespace accepted a duplicate provider idempotency key")
	}

	insertWebhook := fmt.Sprintf(`
		INSERT INTO %s.webhook_events
		(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,sanitized_payload)
		VALUES($1,$2,'provider_a','provider_a','evt_fresh_shared',$3,'{}'::jsonb)`, schema)
	if _, err := pool.Exec(ctx, insertWebhook, "provider_a_primary", "provider_a_primary_v1", "op_fresh_primary"); err != nil {
		t.Fatalf("insert fresh webhook event: %v", err)
	}
	if _, err := pool.Exec(ctx, insertWebhook, "provider_a", legacyConnectionRevision, "op_fresh_alias"); err == nil {
		t.Fatal("same namespace accepted a duplicate provider webhook event ID")
	}

	insertOutbox := fmt.Sprintf(`
		INSERT INTO %s.outbox
		(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,payload)
		VALUES($1,$2,'provider_a','provider_a','evt_outbox_shared',$3,'{}'::jsonb)`, schema)
	if _, err := pool.Exec(ctx, insertOutbox, "provider_a_primary", "provider_a_primary_v1", "op_fresh_primary"); err != nil {
		t.Fatalf("insert fresh outbox event: %v", err)
	}
	if _, err := pool.Exec(ctx, insertOutbox, "provider_a", legacyConnectionRevision, "op_fresh_alias"); err == nil {
		t.Fatal("same namespace accepted a duplicate gateway outbox event ID")
	}
}

func TestGatewayMigration0001To0002PreservesLegacyData(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	schema := fmt.Sprintf("provider_gateway_upgrade_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	v1 := strings.ReplaceAll(initialMigration, "provider_gateway", schema)
	v2 := strings.ReplaceAll(providerConnectionMigration, "provider_gateway", schema)
	if _, err := pool.Exec(ctx, v1); err != nil {
		t.Fatalf("apply isolated 0001: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	input := OperationRequest{
		OperationID: "op_legacy_upgrade", AttemptID: "att_legacy_upgrade",
		ProviderConnectionID: "provider_legacy", ProviderConnectionRevision: legacyConnectionRevision,
		ProviderNamespaceID: "provider_legacy", Provider: "provider_legacy",
		AmountMinor: 1250, Currency: "USD", PaymentMethodToken: "pm_legacy",
		CaptureMethod: "automatic",
	}
	legacyOutcome := map[string]any{
		"event_id": "evt_legacy_upgrade", "operation_id": input.OperationID,
		"provider": input.Provider, "status": StatusUnknown,
		"side_effect_certainty": "possible", "failure_code": "provider_response_unknown",
		"occurred_at": now,
	}
	legacyPayload, err := json.Marshal(legacyOutcome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.operations
		(operation_id,attempt_id,provider,provider_idempotency_key,request_hash,status,side_effect_certainty,provider_reference,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,'succeeded','confirmed','ref_legacy_collision',$6,$6)`, schema),
		input.OperationID, input.AttemptID, input.Provider, input.OperationID,
		operationHashV1(input), now,
	); err != nil {
		t.Fatalf("seed isolated 0001 operation: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.operations
		(operation_id,attempt_id,provider,provider_idempotency_key,request_hash,status,side_effect_certainty,provider_reference,created_at,updated_at)
		VALUES('op_legacy_collision_peer','att_legacy_collision_peer',$1,'op_legacy_collision_peer','legacy_peer_hash',
		       'succeeded','confirmed','ref_legacy_collision',$2,$2)`, schema), input.Provider, now.Add(time.Microsecond)); err != nil {
		t.Fatalf("seed isolated duplicate reference operation: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.webhook_events(provider,event_id,operation_id,sanitized_payload)
		VALUES($1,$2,$3,$4)`, schema), input.Provider, legacyOutcome["event_id"], input.OperationID, legacyPayload); err != nil {
		t.Fatalf("seed isolated 0001 webhook: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.outbox(provider,event_id,operation_id,payload)
		VALUES($1,$2,$3,$4)`, schema), input.Provider, legacyOutcome["event_id"], input.OperationID, legacyPayload); err != nil {
		t.Fatalf("seed isolated 0001 outbox: %v", err)
	}
	if _, err := pool.Exec(ctx, v2); err != nil {
		t.Fatalf("apply isolated 0002: %v", err)
	}
	var adoptedRevisionCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.provider_connection_revisions`, schema)).Scan(&adoptedRevisionCount); err != nil {
		t.Fatal(err)
	}
	if adoptedRevisionCount != 0 {
		t.Fatal("migration guessed an execution configuration for a legacy connection")
	}

	var connectionID, revision, namespaceID, requestHash string
	var hashVersion int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT provider_connection_id,provider_connection_revision,provider_namespace_id,request_hash_version,request_hash
			FROM %s.operations WHERE operation_id=$1`, schema), input.OperationID).
		Scan(&connectionID, &revision, &namespaceID, &hashVersion, &requestHash); err != nil {
		t.Fatal(err)
	}
	if connectionID != input.Provider || revision != legacyConnectionRevision || namespaceID != input.Provider ||
		hashVersion != requestHashVersionLegacy || requestHash != operationHashV1(input) {
		t.Fatalf("legacy operation migration connection=%q revision=%q namespace=%q version=%d hash=%q", connectionID, revision, namespaceID, hashVersion, requestHash)
	}

	currentOutcome := ProviderOutcome{
		EventID: legacyOutcome["event_id"].(string), OperationID: input.OperationID,
		ProviderConnectionID: input.Provider, ProviderConnectionRevision: legacyConnectionRevision,
		ProviderNamespaceID: input.Provider, Provider: input.Provider,
		Status: StatusUnknown, SideEffectCertainty: "possible",
		FailureCode: "provider_response_unknown", OccurredAt: now,
	}
	for _, table := range []string{"webhook_events", "outbox"} {
		column := "sanitized_payload"
		if table == "outbox" {
			column = "payload"
		}
		var matches bool
		if err := pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT %s = $1::jsonb FROM %s.%s WHERE event_id=$2`, column, schema, table),
			mustJSON(currentOutcome), currentOutcome.EventID,
		).Scan(&matches); err != nil {
			t.Fatal(err)
		}
		if !matches {
			t.Fatalf("%s JSON payload was not upgraded with provider_connection_id", table)
		}
	}
	var reviewCount, claimCount, observationCount, collisionOutboxCount int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FROM %s.operations
		WHERE provider_namespace_id=$1 AND provider_reference='ref_legacy_collision'
		  AND status='review_required' AND failure_code='provider_reference_collision'`, schema), input.Provider).Scan(&reviewCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FROM %s.provider_reference_claims
		WHERE provider_namespace_id=$1 AND provider_reference='ref_legacy_collision'`, schema), input.Provider).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FROM %s.provider_reference_observations
		WHERE provider_namespace_id=$1 AND provider_reference='ref_legacy_collision'`, schema), input.Provider).Scan(&observationCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FROM %s.outbox
		WHERE provider_namespace_id=$1 AND event_id LIKE 'migration_provider_reference_collision_%%'
		  AND payload->>'provider_connection_revision'='legacy_v1'
		  AND payload->>'provider_namespace_id'=$1`, schema), input.Provider).Scan(&collisionOutboxCount); err != nil {
		t.Fatal(err)
	}
	if reviewCount != 2 || claimCount != 1 || observationCount != 2 || collisionOutboxCount != 2 {
		t.Fatalf("collision repair review=%d claims=%d observations=%d outbox=%d", reviewCount, claimCount, observationCount, collisionOutboxCount)
	}
}

func TestPostgresStoreAtomicallyUpgradesLegacyRequestHashWithoutConnectionDrift(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	seedLegacy := func(input OperationRequest) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO provider_gateway.operations
			(operation_id,attempt_id,provider_connection_id,provider_connection_revision,provider_namespace_id,provider,provider_idempotency_key,
			 request_hash,request_hash_version,status,side_effect_certainty,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$3,$1,$6,$7,'created','none',$8,$8)`,
			input.OperationID, input.AttemptID, input.Provider, legacyConnectionRevision, input.Provider,
			operationHashV1(input), requestHashVersionLegacy, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	incomingOperation := func(input OperationRequest) Operation {
		return Operation{
			OperationID: input.OperationID, AttemptID: input.AttemptID,
			ProviderConnectionID: input.ProviderConnectionID, ProviderConnectionRevision: input.ProviderConnectionRevision,
			ProviderNamespaceID: input.ProviderNamespaceID, Provider: input.Provider,
			ProviderIdempotencyKey: input.OperationID,
			RequestHash:            operationHashV2(input), RequestHashVersion: requestHashVersionConnection,
			LegacyRequestHash: operationHashV1(input), Status: StatusCreated,
			SideEffectCertainty: "none", CreatedAt: now, UpdatedAt: now,
		}
	}

	exact := testOperationRequest("op_pg_legacy_exact_"+fmt.Sprint(now.UnixNano()), "provider_legacy")
	exact.ProviderConnectionRevision = legacyConnectionRevision
	seedLegacy(exact)
	type legacyUpgradeResult struct {
		operation Operation
		created   bool
		err       error
	}
	const concurrentLegacyRetries = 8
	upgrades := make(chan legacyUpgradeResult, concurrentLegacyRetries)
	for index := 0; index < concurrentLegacyRetries; index++ {
		go func() {
			upgraded, created, err := store.CreateOrGet(ctx, incomingOperation(exact))
			upgrades <- legacyUpgradeResult{operation: upgraded, created: created, err: err}
		}()
	}
	for index := 0; index < concurrentLegacyRetries; index++ {
		result := <-upgrades
		if result.err != nil || result.created || result.operation.RequestHashVersion != requestHashVersionConnection ||
			result.operation.RequestHash != operationHashV2(exact) {
			t.Fatalf("concurrent exact legacy upgrade: created=%v operation=%+v err=%v", result.created, result.operation, result.err)
		}
	}

	driftedLegacy := testOperationRequest("op_pg_legacy_drift_"+fmt.Sprint(now.UnixNano()), "provider_legacy")
	driftedLegacy.ProviderConnectionRevision = legacyConnectionRevision
	seedLegacy(driftedLegacy)
	drifted := driftedLegacy
	drifted.ProviderConnectionID = "provider_legacy_eu_backup"
	drifted.ProviderConnectionRevision = "provider_legacy_eu_backup_v1"
	if _, _, err := store.CreateOrGet(ctx, incomingOperation(drifted)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy connection drift error=%v, want ErrIdempotencyConflict", err)
	}

	revisionLegacy := testOperationRequest("op_pg_legacy_revision_"+fmt.Sprint(now.UnixNano()), "provider_legacy")
	revisionLegacy.ProviderConnectionRevision = legacyConnectionRevision
	seedLegacy(revisionLegacy)
	revisionDrift := revisionLegacy
	revisionDrift.ProviderConnectionRevision = "provider_legacy_v2"
	if _, _, err := store.CreateOrGet(ctx, incomingOperation(revisionDrift)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy revision drift error=%v, want ErrIdempotencyConflict", err)
	}

	namespaceLegacy := testOperationRequest("op_pg_legacy_namespace_"+fmt.Sprint(now.UnixNano()), "provider_legacy")
	namespaceLegacy.ProviderConnectionRevision = legacyConnectionRevision
	seedLegacy(namespaceLegacy)
	namespaceDrift := namespaceLegacy
	namespaceDrift.ProviderNamespaceID = "provider_legacy_other_namespace"
	if _, _, err := store.CreateOrGet(ctx, incomingOperation(namespaceDrift)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("legacy namespace drift error=%v, want ErrIdempotencyConflict", err)
	}
}

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
		OperationID: "op_it_" + suffix, AttemptID: "att_it_" + suffix,
		ProviderConnectionID: "provider_it_us_primary", ProviderConnectionRevision: "provider_it_us_primary_v1",
		ProviderNamespaceID: "provider_it", Provider: "provider_it",
		ProviderIdempotencyKey: "op_it_" + suffix, RequestHash: "hash_it",
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: "legacy_hash_it", Status: StatusCreated,
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
		EventID: "evt_wrong_provider_" + suffix, OperationID: op.OperationID,
		ProviderConnectionID: op.ProviderConnectionID, Provider: "another_provider",
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "wrong_provider_ref_" + suffix, OccurredAt: now,
	}
	if _, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(wrongProviderOutcome)); !errors.Is(err, ErrNotFound) || inserted {
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
		EventID: "evt_it_" + suffix, OperationID: op.OperationID,
		ProviderConnectionID: op.ProviderConnectionID, Provider: op.Provider,
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_it_" + suffix, OccurredAt: now,
	}
	storedOutcome, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(outcome))
	if err != nil || !inserted || storedOutcome.Status != StatusSucceeded {
		t.Fatalf("apply webhook: inserted=%v outcome=%+v err=%v", inserted, storedOutcome, err)
	}
	_, inserted, err = store.ApplyWebhook(ctx, testWebhookEvent(outcome))
	if err != nil || inserted {
		t.Fatalf("duplicate webhook: inserted=%v err=%v", inserted, err)
	}
	var pendingCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM provider_gateway.outbox WHERE delivered_at IS NULL`).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	items, err := store.PendingOutbox(ctx, pendingCount)
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
		OperationID: "op_monotonic_" + suffix, AttemptID: "att_monotonic_" + suffix,
		ProviderConnectionID: "provider_it_us_primary", ProviderConnectionRevision: "provider_it_us_primary_v1",
		ProviderNamespaceID: "provider_it", Provider: "provider_it",
		ProviderIdempotencyKey: "op_monotonic_" + suffix, RequestHash: "hash_it",
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: "legacy_hash_it", Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_a_" + suffix, CreatedAt: now, UpdatedAt: now,
	}
	if _, inserted, err := store.CreateOrGet(ctx, op); err != nil || !inserted {
		t.Fatalf("create operation: inserted=%v err=%v", inserted, err)
	}

	lateAuthorization := ProviderOutcome{
		EventID: "evt_late_auth_" + suffix, OperationID: op.OperationID,
		ProviderConnectionID: op.ProviderConnectionID, Provider: op.Provider,
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "provider_ref_a_" + suffix, OccurredAt: now,
	}
	stored, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(lateAuthorization))
	if err != nil || !inserted {
		t.Fatalf("late authorization: inserted=%v err=%v", inserted, err)
	}
	if stored.Status != StatusSucceeded || stored.SideEffectCertainty != "confirmed" {
		t.Fatalf("late authorization downgraded capture: %+v", stored)
	}

	conflictingDuplicate := lateAuthorization
	conflictingDuplicate.ProviderReference = "provider_ref_conflicting_duplicate_" + suffix
	if _, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(conflictingDuplicate)); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("reused event: inserted=%v err=%v, want ErrWebhookEventConflict", inserted, err)
	}

	referenceConflict := ProviderOutcome{
		EventID: "evt_ref_conflict_" + suffix, OperationID: op.OperationID,
		ProviderConnectionID: op.ProviderConnectionID, Provider: op.Provider,
		Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: "provider_ref_b_" + suffix, OccurredAt: now,
	}
	stored, inserted, err = store.ApplyWebhook(ctx, testWebhookEvent(referenceConflict))
	if err != nil || !inserted {
		t.Fatalf("reference conflict: inserted=%v err=%v", inserted, err)
	}
	if stored.Status != StatusReview || stored.ProviderReference != "provider_ref_a_"+suffix {
		t.Fatalf("reference conflict was not quarantined: %+v", stored)
	}
}

func TestPostgresStoreNamespaceScopedWebhookAndReferenceCollisions(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	now := time.Now().UTC()
	primaryIdentity := ProviderConnectionIdentity{Provider: "provider_it", ConnectionRevision: "provider_it_us_primary_v1", NamespaceID: "provider_it"}
	backupIdentity := ProviderConnectionIdentity{Provider: "provider_it", ConnectionRevision: "provider_it_eu_backup_v1", NamespaceID: "provider_it"}
	seed := func(operationID, connectionID string, identity ProviderConnectionIdentity) {
		t.Helper()
		_, inserted, seedErr := store.CreateOrGet(ctx, Operation{
			OperationID: operationID, AttemptID: "att_" + operationID,
			ProviderConnectionID: connectionID, ProviderConnectionRevision: identity.ConnectionRevision,
			ProviderNamespaceID: identity.NamespaceID, Provider: identity.Provider,
			ProviderIdempotencyKey: operationID, RequestHash: "hash_" + operationID,
			RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: "legacy_" + operationID,
			Status: StatusUnknown, SideEffectCertainty: "possible", CreatedAt: now, UpdatedAt: now,
		})
		if seedErr != nil || !inserted {
			t.Fatalf("seed %s inserted=%v err=%v", operationID, inserted, seedErr)
		}
	}
	primaryOperationID := "op_namespace_primary_" + suffix
	backupOperationID := "op_namespace_backup_" + suffix
	seed(primaryOperationID, "provider_it_us_primary", primaryIdentity)
	seed(backupOperationID, "provider_it_eu_backup", backupIdentity)

	providerEventID := "evt_namespace_shared_" + suffix
	aliasIdentity := ProviderConnectionIdentity{Provider: "provider_it", ConnectionRevision: legacyConnectionRevision, NamespaceID: "provider_it"}
	aliasEvent := VerifiedWebhookEvent{
		PathConnectionID: "provider_it", PathIdentity: aliasIdentity,
		AllowedOperationIdentities: map[string]ProviderConnectionIdentity{
			"provider_it": aliasIdentity, "provider_it_us_primary": primaryIdentity,
		},
		EventID: providerEventID, OperationID: primaryOperationID, Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "ref_namespace_primary_" + suffix, OccurredAt: now,
	}
	firstOutcome, inserted, err := store.ApplyWebhook(ctx, aliasEvent)
	if err != nil || !inserted || firstOutcome.ProviderConnectionID != "provider_it_us_primary" ||
		firstOutcome.ProviderConnectionRevision != primaryIdentity.ConnectionRevision {
		t.Fatalf("alias event inserted=%v outcome=%+v err=%v", inserted, firstOutcome, err)
	}
	canonicalReplay := aliasEvent
	canonicalReplay.PathConnectionID = "provider_it_us_primary"
	canonicalReplay.PathIdentity = primaryIdentity
	canonicalReplay.AllowedOperationIdentities = map[string]ProviderConnectionIdentity{"provider_it_us_primary": primaryIdentity}
	if _, inserted, err := store.ApplyWebhook(ctx, canonicalReplay); err != nil || inserted {
		t.Fatalf("canonical replay inserted=%v err=%v", inserted, err)
	}

	conflictingEvent := VerifiedWebhookEvent{
		PathConnectionID: "provider_it_eu_backup", PathIdentity: backupIdentity,
		AllowedOperationIdentities: map[string]ProviderConnectionIdentity{"provider_it_eu_backup": backupIdentity},
		EventID:                    providerEventID, OperationID: backupOperationID, Status: StatusSucceeded,
		SideEffectCertainty: "confirmed", ProviderReference: "ref_namespace_backup_" + suffix, OccurredAt: now,
	}
	if _, inserted, err := store.ApplyWebhook(ctx, conflictingEvent); !errors.Is(err, ErrWebhookEventConflict) || inserted {
		t.Fatalf("namespace event conflict inserted=%v err=%v", inserted, err)
	}
	for _, operationID := range []string{primaryOperationID, backupOperationID} {
		op, getErr := store.Get(ctx, operationID)
		if getErr != nil || op.Status != StatusReview || op.FailureCode != "webhook_event_collision" {
			t.Fatalf("event collision operation %s = %+v err=%v", operationID, op, getErr)
		}
	}
	var conflictEvidenceCount int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.webhook_event_conflicts
		WHERE provider_namespace_id='provider_it' AND event_id=$1`, providerEventID).Scan(&conflictEvidenceCount); err != nil {
		t.Fatal(err)
	}
	if conflictEvidenceCount != 1 {
		t.Fatalf("webhook conflict evidence count=%d, want 1", conflictEvidenceCount)
	}
	for _, operationID := range []string{primaryOperationID, backupOperationID} {
		retainedReview, err := store.SetStatus(ctx, operationID, StatusSucceeded, "confirmed", "ref_after_event_collision_"+suffix, "")
		if err != nil || retainedReview.Status != StatusReview || retainedReview.FailureCode != "webhook_event_collision" {
			t.Fatalf("reference collision erased earlier event review: operation=%+v err=%v", retainedReview, err)
		}
	}

	firstReferenceOperationID := "op_reference_primary_" + suffix
	secondReferenceOperationID := "op_reference_backup_" + suffix
	seed(firstReferenceOperationID, "provider_it_us_primary", primaryIdentity)
	seed(secondReferenceOperationID, "provider_it_eu_backup", backupIdentity)
	sharedReference := "ref_namespace_collision_" + suffix
	if _, err := store.SetStatus(ctx, firstReferenceOperationID, StatusSucceeded, "confirmed", sharedReference, ""); err != nil {
		t.Fatal(err)
	}
	secondResult, err := store.SetStatus(ctx, secondReferenceOperationID, StatusSucceeded, "confirmed", sharedReference, "")
	if err != nil || secondResult.Status != StatusReview {
		t.Fatalf("reference collision second=%+v err=%v", secondResult, err)
	}
	var observationCount, ownerReviewCount, collisionSignalCount int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.provider_reference_observations
		WHERE provider_namespace_id='provider_it' AND provider_reference=$1`, sharedReference).Scan(&observationCount); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.operations
		WHERE operation_id=ANY($1) AND status='review_required' AND failure_code='provider_reference_collision'`,
		[]string{firstReferenceOperationID, secondReferenceOperationID}).Scan(&ownerReviewCount); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.outbox
		WHERE provider_namespace_id='provider_it' AND operation_id=$1
		  AND payload->>'failure_code'='provider_reference_collision'`, firstReferenceOperationID).Scan(&collisionSignalCount); err != nil {
		t.Fatal(err)
	}
	if observationCount != 2 || ownerReviewCount != 2 || collisionSignalCount != 1 {
		t.Fatalf("reference collision observations=%d reviewed=%d signals=%d", observationCount, ownerReviewCount, collisionSignalCount)
	}
	retainedReview, err := store.SetStatus(ctx, firstReferenceOperationID, StatusSucceeded, "confirmed", sharedReference, "")
	if err != nil || retainedReview.Status != StatusReview || retainedReview.FailureCode != "provider_reference_collision" {
		t.Fatalf("later matching evidence erased durable collision review: operation=%+v err=%v", retainedReview, err)
	}
	for _, operationID := range []string{firstReferenceOperationID, secondReferenceOperationID} {
		identity := primaryIdentity
		connectionID := "provider_it_us_primary"
		if operationID == secondReferenceOperationID {
			identity, connectionID = backupIdentity, "provider_it_eu_backup"
		}
		_, inserted, err := store.ApplyWebhook(ctx, VerifiedWebhookEvent{
			PathConnectionID: connectionID, PathIdentity: identity,
			AllowedOperationIdentities: map[string]ProviderConnectionIdentity{connectionID: identity},
			EventID:                    "evt_after_reference_collision_" + suffix, OperationID: operationID,
			Status: StatusSucceeded, SideEffectCertainty: "confirmed", ProviderReference: sharedReference, OccurredAt: now,
		})
		if operationID == firstReferenceOperationID {
			if err != nil || !inserted {
				t.Fatalf("first reference-review webhook inserted=%v err=%v", inserted, err)
			}
		} else if !errors.Is(err, ErrWebhookEventConflict) || inserted {
			t.Fatalf("second reference-review webhook inserted=%v err=%v", inserted, err)
		}
		retainedReview, getErr := store.Get(ctx, operationID)
		if getErr != nil || retainedReview.FailureCode != "provider_reference_collision" {
			t.Fatalf("event collision erased earlier reference review: operation=%+v err=%v", retainedReview, getErr)
		}
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
			OperationID: id, AttemptID: "att_" + id,
			ProviderConnectionID: "provider_it_us_primary", ProviderConnectionRevision: "provider_it_us_primary_v1",
			ProviderNamespaceID: "provider_it", Provider: "provider_it", ProviderIdempotencyKey: id,
			RequestHash: "hash_" + id, RequestHashVersion: requestHashVersionConnection,
			LegacyRequestHash: "legacy_hash_" + id, Status: status, SideEffectCertainty: certainty, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil || !inserted {
			t.Fatalf("seed %s: inserted=%v err=%v", id, inserted, err)
		}
	}

	declineFirstID := "op_decline_auth_" + suffix
	seed(declineFirstID, StatusFailed, "none")
	lateAuthorization, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(ProviderOutcome{
		EventID: "evt_decline_auth_" + suffix, OperationID: declineFirstID,
		ProviderConnectionID: "provider_it_us_primary", Provider: "provider_it",
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "auth_ref_" + suffix,
		FailureCode: "authorization_only", OccurredAt: now,
	}))
	if err != nil || !inserted || lateAuthorization.Status != StatusReview || lateAuthorization.FailureCode != "authorization_failure_conflict" {
		t.Fatalf("decline then authorization: inserted=%v outcome=%+v err=%v", inserted, lateAuthorization, err)
	}

	authFirstID := "op_auth_decline_" + suffix
	seed(authFirstID, StatusSending, "possible")
	authorization, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(ProviderOutcome{
		EventID: "evt_auth_first_" + suffix, OperationID: authFirstID,
		ProviderConnectionID: "provider_it_us_primary", Provider: "provider_it",
		Status: StatusUnknown, SideEffectCertainty: "possible", ProviderReference: "auth_first_ref_" + suffix,
		FailureCode: "authorization_only", OccurredAt: now,
	}))
	if err != nil || !inserted || authorization.Status != StatusUnknown {
		t.Fatalf("authorization first: inserted=%v outcome=%+v err=%v", inserted, authorization, err)
	}
	decline, inserted, err := store.ApplyWebhook(ctx, testWebhookEvent(ProviderOutcome{
		EventID: "evt_decline_second_" + suffix, OperationID: authFirstID,
		ProviderConnectionID: "provider_it_us_primary", Provider: "provider_it",
		Status: StatusFailed, SideEffectCertainty: "none", ProviderReference: authorization.ProviderReference,
		FailureCode: "do_not_honor", OccurredAt: now.Add(time.Second),
	}))
	if err != nil || !inserted || decline.Status != StatusReview || decline.FailureCode != "authorization_failure_conflict" {
		t.Fatalf("authorization then decline: inserted=%v outcome=%+v err=%v", inserted, decline, err)
	}
}
