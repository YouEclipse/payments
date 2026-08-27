package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("operation not found")
var ErrIdempotencyConflict = errors.New("operation id was reused with a different request")
var ErrWebhookEventConflict = errors.New("webhook event id was reused with different event data")
var ErrUnconfiguredConnection = errors.New("durable operation provider connection identity is not configured")
var ErrConnectionRevisionConflict = errors.New("provider connection revision is already bound to different execution configuration")

const gatewayMigrationLockID int64 = 710_130_613

type Store interface {
	CreateOrGet(context.Context, Operation) (Operation, bool, error)
	Get(context.Context, string) (Operation, error)
	ClaimDispatch(context.Context, Operation, time.Duration) (Operation, bool, error)
	DispatchInFlight(context.Context, string) (bool, error)
	SetStatus(context.Context, string, string, string, string, string) (Operation, error)
	ApplyProviderObservation(context.Context, ProviderOperationObservation, *Operation) (Operation, bool, error)
	ListProviderObservations(context.Context, string) ([]ProviderOperationObservation, error)
	ApplyWebhook(context.Context, VerifiedWebhookEvent) (ProviderOutcome, bool, error)
	PendingOutbox(context.Context, int) ([]OutboxItem, error)
	MarkOutboxDelivered(context.Context, int64) error
	Ping(context.Context) error
	ValidateNonterminalConnections(context.Context, map[string]ProviderConnectionIdentity) error
	BindConnectionRevisions(context.Context, map[string]ProviderConnectionRevisionBinding) error
}

//go:embed migrations/0001_init.sql
var initialMigration string

//go:embed migrations/0002_provider_connection_identity.sql
var providerConnectionMigration string

//go:embed migrations/0003_recovery_invariants.sql
var recoveryInvariantMigration string

//go:embed migrations/0004_provider_operation_observations.sql
var providerObservationMigration string

//go:embed migrations/0005_append_only_truncate_guards.sql
var appendOnlyGuardMigration string

type PostgresStore struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, databaseURL string, migrate bool) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if migrate {
		if err := migrateGateway(ctx, pool); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return &PostgresStore{pool: pool}, nil
}

func migrateGateway(ctx context.Context, pool *pgxpool.Pool) error {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire provider gateway migration connection: %w", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock($1)`, gatewayMigrationLockID); err != nil {
		return fmt.Errorf("acquire provider gateway migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, gatewayMigrationLockID)
	}()

	if _, err := connection.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS provider_gateway;
		CREATE TABLE IF NOT EXISTS provider_gateway.schema_migrations (
			version text PRIMARY KEY,
			checksum bytea NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create provider gateway migration ledger: %w", err)
	}

	for _, migration := range []struct {
		version string
		body    string
	}{
		{version: "0001_init.sql", body: initialMigration},
		{version: "0002_provider_connection_identity.sql", body: providerConnectionMigration},
		{version: "0003_recovery_invariants.sql", body: recoveryInvariantMigration},
		{version: "0004_provider_operation_observations.sql", body: providerObservationMigration},
		{version: "0005_append_only_truncate_guards.sql", body: appendOnlyGuardMigration},
	} {
		checksum := sha256.Sum256([]byte(migration.body))
		var storedChecksum []byte
		err = connection.QueryRow(ctx,
			`SELECT checksum FROM provider_gateway.schema_migrations WHERE version=$1`, migration.version,
		).Scan(&storedChecksum)
		if err == nil {
			if !bytes.Equal(storedChecksum, checksum[:]) {
				return fmt.Errorf("provider gateway migration %s checksum does not match the applied version", migration.version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check provider gateway migration %s: %w", migration.version, err)
		}

		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin provider gateway migration %s: %w", migration.version, err)
		}
		if _, err := tx.Exec(ctx, migration.body); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply provider gateway migration %s: %w", migration.version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO provider_gateway.schema_migrations(version,checksum) VALUES($1,$2)`, migration.version, checksum[:],
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record provider gateway migration %s: %w", migration.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit provider gateway migration %s: %w", migration.version, err)
		}
	}
	return nil
}

func (s *PostgresStore) Close() { s.pool.Close() }
func (s *PostgresStore) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return err
	}
	// Readiness requires the owned schema, not merely a reachable PostgreSQL
	// server. This fails closed when migrations were not applied.
	if _, err := s.pool.Exec(ctx, `SELECT 1 FROM provider_gateway.operations LIMIT 0`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `SELECT 1 FROM provider_gateway.provider_operation_observations LIMIT 0`)
	return err
}

func (s *PostgresStore) ValidateNonterminalConnections(ctx context.Context, configured map[string]ProviderConnectionIdentity) error {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT provider_connection_id, provider_connection_revision, provider_namespace_id, provider
		FROM provider_gateway.operations
		WHERE status IN ('created','sending','unknown')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var connectionID, revision, namespaceID, provider string
		if err := rows.Scan(&connectionID, &revision, &namespaceID, &provider); err != nil {
			return err
		}
		configuredIdentity, ok := configured[connectionID]
		if !ok || configuredIdentity.Provider != provider || configuredIdentity.ConnectionRevision != revision || configuredIdentity.NamespaceID != namespaceID {
			return fmt.Errorf("%w: connection=%q durable_revision=%q durable_namespace=%q durable_provider=%q configured=%+v",
				ErrUnconfiguredConnection, connectionID, revision, namespaceID, provider, configuredIdentity)
		}
	}
	return rows.Err()
}

func (s *PostgresStore) BindConnectionRevisions(ctx context.Context, configured map[string]ProviderConnectionRevisionBinding) error {
	connectionIDs, err := orderedConnectionRevisionIDs(configured)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, connectionID := range connectionIDs {
		binding := configured[connectionID]
		if _, err := tx.Exec(ctx, `
			INSERT INTO provider_gateway.provider_connection_revisions
			(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,fingerprint_version,config_fingerprint,conclusive_absence)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(provider_connection_id,provider_connection_revision) DO NOTHING`,
			connectionID, binding.ConnectionRevision, binding.NamespaceID, binding.Provider,
			binding.FingerprintVersion, binding.ConfigFingerprint, binding.ConclusiveAbsence); err != nil {
			return err
		}
		// 0003 deliberately leaves pre-capability rows NULL. The first new binary
		// adopts one explicit value; the database trigger rejects every rewrite.
		if _, err := tx.Exec(ctx, `
			UPDATE provider_gateway.provider_connection_revisions
			SET conclusive_absence=$3
			WHERE provider_connection_id=$1 AND provider_connection_revision=$2
			  AND conclusive_absence IS NULL`, connectionID, binding.ConnectionRevision, binding.ConclusiveAbsence); err != nil {
			return err
		}
		var stored ProviderConnectionRevisionBinding
		stored.ConnectionRevision = binding.ConnectionRevision
		if err := tx.QueryRow(ctx, `
			SELECT provider_namespace_id,provider,fingerprint_version,config_fingerprint,conclusive_absence
			FROM provider_gateway.provider_connection_revisions
			WHERE provider_connection_id=$1 AND provider_connection_revision=$2`,
			connectionID, binding.ConnectionRevision).Scan(
			&stored.NamespaceID, &stored.Provider, &stored.FingerprintVersion, &stored.ConfigFingerprint, &stored.ConclusiveAbsence,
		); err != nil {
			return err
		}
		if stored != binding {
			return fmt.Errorf("%w: connection=%q revision=%q", ErrConnectionRevisionConflict, connectionID, binding.ConnectionRevision)
		}
	}
	return tx.Commit(ctx)
}

func orderedConnectionRevisionIDs(configured map[string]ProviderConnectionRevisionBinding) ([]string, error) {
	connectionIDs := make([]string, 0, len(configured))
	for connectionID, binding := range configured {
		if connectionID == "" || binding.ConnectionRevision == "" || binding.NamespaceID == "" || binding.Provider == "" ||
			binding.FingerprintVersion <= 0 || len(binding.ConfigFingerprint) != sha256.Size*2 {
			return nil, errors.New("provider connection revision binding is incomplete")
		}
		for _, character := range binding.ConfigFingerprint {
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return nil, errors.New("provider connection revision fingerprint must be lowercase SHA-256 hexadecimal")
			}
		}
		connectionIDs = append(connectionIDs, connectionID)
	}
	// Every concurrent startup takes registry keys in the same order. No request
	// ever updates a binding, and a conflicting batch rolls back atomically.
	sort.Strings(connectionIDs)
	return connectionIDs, nil
}

func (s *PostgresStore) CreateOrGet(ctx context.Context, op Operation) (Operation, bool, error) {
	if err := validateCurrentRequestHashes(op); err != nil {
		return Operation{}, false, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO provider_gateway.operations
		(operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		 provider, provider_idempotency_key, request_hash, request_hash_version, status, side_effect_certainty,
		 first_dispatched_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)
		ON CONFLICT (operation_id) DO NOTHING`,
		op.OperationID, op.AttemptID, op.ProviderConnectionID, op.ProviderConnectionRevision, op.ProviderNamespaceID,
		op.Provider, op.ProviderIdempotencyKey, op.RequestHash, op.RequestHashVersion, op.Status, op.SideEffectCertainty,
		op.FirstDispatchedAt, op.CreatedAt)
	if err != nil {
		return Operation{}, false, err
	}
	if tag.RowsAffected() == 1 {
		created, err := s.Get(ctx, op.OperationID)
		return created, true, err
	}

	// Rows created before provider connection identity existed retain the v1
	// request hash. Upgrade only the exact legacy identity and exact v1 payload
	// in one conditional UPDATE; a same-provider move to another connection must
	// remain an idempotency conflict.
	upgraded, err := scanGatewayOperation(s.pool.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET request_hash=$2, request_hash_version=$3
		WHERE operation_id=$1
		  AND request_hash_version=$4
		  AND provider_connection_id=provider
		  AND provider_connection_revision=$5
		  AND provider_connection_revision='legacy_v1'
		  AND provider_namespace_id=provider
		  AND provider_connection_id=$6
		  AND provider_namespace_id=$7
		  AND provider=$8
		  AND request_hash=$9
		  AND attempt_id=$10
		  AND provider_idempotency_key=$11
		RETURNING operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id, provider,
		          provider_idempotency_key, request_hash, request_hash_version, status,
		          side_effect_certainty, COALESCE(provider_reference,''),
		          COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at`,
		op.OperationID, op.RequestHash, requestHashVersionConnection, requestHashVersionLegacy,
		op.ProviderConnectionRevision, op.ProviderConnectionID, op.ProviderNamespaceID, op.Provider,
		op.LegacyRequestHash, op.AttemptID, op.ProviderIdempotencyKey,
	))
	if err == nil {
		return upgraded, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, false, err
	}

	existing, err := s.Get(ctx, op.OperationID)
	if err != nil {
		return Operation{}, false, err
	}
	if existing.RequestHashVersion != requestHashVersionConnection ||
		existing.RequestHash != op.RequestHash ||
		existing.AttemptID != op.AttemptID ||
		existing.ProviderConnectionID != op.ProviderConnectionID ||
		existing.ProviderConnectionRevision != op.ProviderConnectionRevision ||
		existing.ProviderNamespaceID != op.ProviderNamespaceID ||
		existing.Provider != op.Provider ||
		existing.ProviderIdempotencyKey != op.ProviderIdempotencyKey {
		return Operation{}, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Operation, error) {
	op, err := scanGatewayOperation(s.pool.QueryRow(ctx, `
		SELECT operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		       provider, provider_idempotency_key, request_hash, request_hash_version, status,
		       side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at
		FROM provider_gateway.operations WHERE operation_id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return op, err
}

// ClaimDispatch is the last durable ownership check before Provider I/O. The
// complete immutable command plus the previously observed status/timestamp is
// a compare-and-swap token. A concurrent worker may observe Sending, but only
// the worker that advances updated_at may cross the Provider boundary.
func (s *PostgresStore) ClaimDispatch(ctx context.Context, expected Operation, lease time.Duration) (Operation, bool, error) {
	if lease <= 0 || (expected.Status != StatusCreated && expected.Status != StatusSending && expected.Status != StatusUnknown) {
		current, err := s.Get(ctx, expected.OperationID)
		return current, false, err
	}
	claimed, err := scanGatewayOperation(s.pool.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET status='sending', side_effect_certainty='possible',
		    first_dispatched_at=COALESCE(first_dispatched_at,GREATEST(clock_timestamp(),created_at)),
		    dispatch_lease_until=clock_timestamp()+($12 * interval '1 microsecond'),
		    updated_at=GREATEST(clock_timestamp(),updated_at+interval '1 microsecond',created_at)
		WHERE operation_id=$1 AND attempt_id=$2
		  AND provider_connection_id=$3 AND provider_connection_revision=$4
		  AND provider_namespace_id=$5 AND provider=$6
		  AND provider_idempotency_key=$7 AND request_hash=$8 AND request_hash_version=$9
		  AND status=$10 AND updated_at=$11
		  AND status IN ('created','sending','unknown')
		  AND (dispatch_lease_until IS NULL OR dispatch_lease_until <= clock_timestamp())
		RETURNING operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id, provider,
		          provider_idempotency_key, request_hash, request_hash_version, status,
		          side_effect_certainty, COALESCE(provider_reference,''),
		          COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at`,
		expected.OperationID, expected.AttemptID, expected.ProviderConnectionID, expected.ProviderConnectionRevision,
		expected.ProviderNamespaceID, expected.Provider, expected.ProviderIdempotencyKey, expected.RequestHash,
		expected.RequestHashVersion, expected.Status, expected.UpdatedAt, lease.Microseconds(),
	))
	if err == nil {
		return claimed, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, false, err
	}
	current, err := s.Get(ctx, expected.OperationID)
	return current, false, err
}

func (s *PostgresStore) DispatchInFlight(ctx context.Context, operationID string) (bool, error) {
	var inFlight bool
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(dispatch_lease_until > clock_timestamp(),false)
		FROM provider_gateway.operations WHERE operation_id=$1`, operationID).Scan(&inFlight)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return inFlight, err
}

func (s *PostgresStore) SetStatus(ctx context.Context, id, status, certainty, reference, failureCode string) (Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	op, collisionOwner, _, err := applyStatusPostgres(ctx, tx, id, status, certainty, reference, failureCode, nil)
	if err != nil {
		return Operation{}, err
	}
	if collisionOwner != nil {
		if err := enqueueReferenceCollisionOutbox(ctx, tx, *collisionOwner, op.OperationID); err != nil {
			return Operation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, err
	}
	return op, nil
}

// SetStatusIfCurrent is the state-only CAS primitive retained for reducer
// tests. Provider boundary code must use ApplyProviderObservation so evidence
// and the state transition share one transaction.
func (s *PostgresStore) SetStatusIfCurrent(ctx context.Context, expected Operation, status, certainty, reference, failureCode string) (Operation, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, false, err
	}
	defer tx.Rollback(ctx)
	op, collisionOwner, applied, err := applyStatusPostgres(
		ctx, tx, expected.OperationID, status, certainty, reference, failureCode, &expected,
	)
	if err != nil {
		return Operation{}, false, err
	}
	if collisionOwner != nil {
		if err := enqueueReferenceCollisionOutbox(ctx, tx, *collisionOwner, op.OperationID); err != nil {
			return Operation{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, false, err
	}
	return op, applied, nil
}

func applyStatusPostgres(ctx context.Context, tx pgx.Tx, id, status, certainty, reference, failureCode string, expected *Operation) (Operation, *Operation, bool, error) {
	locked, err := scanGatewayOperation(tx.QueryRow(ctx, `
		SELECT operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		       provider, provider_idempotency_key, request_hash, request_hash_version, status,
		       side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at
		FROM provider_gateway.operations WHERE operation_id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, nil, false, ErrNotFound
	}
	if err != nil {
		return Operation{}, nil, false, err
	}
	if expected != nil && !dispatchClaimMatches(locked, *expected) {
		return locked, nil, false, nil
	}

	if reference != "" {
		ownerID, err := recordProviderReferencePostgres(ctx, tx, locked, reference)
		if err != nil {
			return Operation{}, nil, false, err
		}
		if ownerID != locked.OperationID {
			owner, err := markProviderReferenceCollision(ctx, tx, ownerID, reference)
			if err != nil {
				return Operation{}, nil, false, err
			}
			current, err := markProviderReferenceCollision(ctx, tx, locked.OperationID, reference)
			if err != nil {
				return Operation{}, nil, false, err
			}
			return current, &owner, true, nil
		}
	}

	updated, err := scanGatewayOperation(tx.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET first_dispatched_at = CASE
		      WHEN first_dispatched_at IS NULL AND $2 = 'sending' THEN GREATEST(clock_timestamp(),created_at)
		      ELSE first_dispatched_at END,
		    dispatch_lease_until = NULL,
		    status = CASE
		      WHEN status = 'review_required' THEN status
		      WHEN status <> 'created' AND $2 = 'created' THEN status
		      WHEN status = 'created' AND $2 IN ('unknown','succeeded','failed') THEN 'review_required'
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN 'review_required'
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'review_required'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'review_required'
		      WHEN status IN ('succeeded','failed') AND $2 IN ('created','sending','unknown') THEN status
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN 'review_required'
		      ELSE $2 END,
		    side_effect_certainty = CASE
		      WHEN status = 'review_required' THEN side_effect_certainty
		      WHEN status <> 'created' AND $2 = 'created' THEN side_effect_certainty
		      WHEN status = 'created' AND $2 IN ('unknown','succeeded','failed') THEN 'possible'
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN 'possible'
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'possible'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'possible'
		      WHEN status IN ('succeeded','failed') AND $2 IN ('created','sending','unknown') THEN side_effect_certainty
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN 'possible'
		      ELSE $3 END,
		    provider_reference=CASE
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN provider_reference
		      ELSE COALESCE(NULLIF($4,''),provider_reference) END,
			    failure_code=CASE
			      WHEN status = 'review_required' THEN failure_code
			      WHEN status = 'created' AND $2 IN ('unknown','succeeded','failed') THEN 'provider_evidence_without_dispatch_record'
			      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN 'provider_reference_conflict'
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'authorization_failure_conflict'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'authorization_failure_conflict'
		      WHEN status='succeeded' AND $2 IN ('created','sending','unknown') THEN NULL
		      WHEN status='failed' AND $2 IN ('created','sending','unknown') THEN failure_code
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN failure_code
		      WHEN $2='succeeded' THEN NULL
		      ELSE COALESCE(NULLIF($5,''),failure_code) END,
		    updated_at=GREATEST(clock_timestamp(),updated_at+interval '1 microsecond',created_at)
		WHERE operation_id=$1
		RETURNING operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		          provider, provider_idempotency_key, request_hash, request_hash_version, status,
		          side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at`,
		id, status, certainty, reference, failureCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, nil, false, ErrNotFound
	}
	return updated, nil, true, err
}

func recordProviderReferencePostgres(ctx context.Context, tx pgx.Tx, operation Operation, reference string) (string, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.provider_reference_observations(provider_namespace_id,provider_reference,operation_id)
		VALUES($1,$2,$3)
		ON CONFLICT(provider_namespace_id,provider_reference,operation_id) DO NOTHING`,
		operation.ProviderNamespaceID, reference, operation.OperationID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.provider_reference_claims(provider_namespace_id,provider_reference,operation_id)
		VALUES($1,$2,$3)
		ON CONFLICT(provider_namespace_id,provider_reference) DO NOTHING`,
		operation.ProviderNamespaceID, reference, operation.OperationID); err != nil {
		return "", err
	}
	var ownerID string
	err := tx.QueryRow(ctx, `
		SELECT operation_id FROM provider_gateway.provider_reference_claims
		WHERE provider_namespace_id=$1 AND provider_reference=$2`,
		operation.ProviderNamespaceID, reference).Scan(&ownerID)
	return ownerID, err
}

func markProviderReferenceCollision(ctx context.Context, tx pgx.Tx, operationID, reference string) (Operation, error) {
	return scanGatewayOperation(tx.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET status='review_required',
		    dispatch_lease_until=NULL,
		    side_effect_certainty='possible',
		    provider_reference=COALESCE(provider_reference,NULLIF($2,'')),
		    failure_code=CASE WHEN status='review_required' AND NULLIF(failure_code,'') IS NOT NULL
		                      THEN failure_code ELSE 'provider_reference_collision' END,
		    updated_at=GREATEST(clock_timestamp(),updated_at+interval '1 microsecond',created_at)
		WHERE operation_id=$1
		RETURNING operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		          provider, provider_idempotency_key, request_hash, request_hash_version, status,
		          side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at`,
		operationID, reference))
}

func enqueueReferenceCollisionOutbox(ctx context.Context, tx pgx.Tx, owner Operation, collidingOperationID string) error {
	outcome := ProviderOutcome{
		EventID:     collisionEventID(owner.ProviderNamespaceID, owner.ProviderReference, owner.OperationID, collidingOperationID),
		OperationID: owner.OperationID, ProviderConnectionID: owner.ProviderConnectionID,
		ProviderConnectionRevision: owner.ProviderConnectionRevision, ProviderNamespaceID: owner.ProviderNamespaceID,
		Provider: owner.Provider, Status: StatusReview, SideEffectCertainty: "possible",
		ProviderReference: owner.ProviderReference, FailureCode: "provider_reference_collision", OccurredAt: time.Now().UTC(),
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.outbox
		(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(provider_namespace_id,event_id) DO NOTHING`,
		outcome.ProviderConnectionID, outcome.ProviderConnectionRevision, outcome.ProviderNamespaceID,
		outcome.Provider, outcome.EventID, outcome.OperationID, mustJSON(outcome))
	return err
}

func collisionEventID(namespaceID, reference, ownerOperationID, collidingOperationID string) string {
	sum := sha256.Sum256(mustJSON([]string{namespaceID, reference, ownerOperationID, collidingOperationID}))
	return "gateway_provider_reference_collision_" + fmt.Sprintf("%x", sum[:16])
}

func markWebhookEventCollision(ctx context.Context, tx pgx.Tx, operationID string) (Operation, error) {
	return scanGatewayOperation(tx.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET status='review_required',
		    dispatch_lease_until=NULL,
		    side_effect_certainty='possible',
		    failure_code=CASE WHEN status='review_required' AND NULLIF(failure_code,'') IS NOT NULL
		                      THEN failure_code ELSE 'webhook_event_collision' END,
		    updated_at=GREATEST(clock_timestamp(),updated_at+interval '1 microsecond',created_at)
		WHERE operation_id=$1
		RETURNING operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		          provider, provider_idempotency_key, request_hash, request_hash_version, status,
		          side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at`,
		operationID))
}

func enqueueWebhookEventCollisionOutbox(ctx context.Context, tx pgx.Tx, operation Operation, providerEventID, payloadHash string) error {
	outcome := ProviderOutcome{
		EventID:     webhookCollisionEventID(operation.ProviderNamespaceID, providerEventID, operation.OperationID, payloadHash),
		OperationID: operation.OperationID, ProviderConnectionID: operation.ProviderConnectionID,
		ProviderConnectionRevision: operation.ProviderConnectionRevision, ProviderNamespaceID: operation.ProviderNamespaceID,
		Provider: operation.Provider, Status: StatusReview, SideEffectCertainty: "possible",
		ProviderReference: operation.ProviderReference, FailureCode: "webhook_event_collision", OccurredAt: time.Now().UTC(),
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.outbox
		(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(provider_namespace_id,event_id) DO NOTHING`,
		outcome.ProviderConnectionID, outcome.ProviderConnectionRevision, outcome.ProviderNamespaceID,
		outcome.Provider, outcome.EventID, outcome.OperationID, mustJSON(outcome))
	return err
}

func webhookCollisionEventID(namespaceID, providerEventID, operationID, payloadHash string) string {
	sum := sha256.Sum256(mustJSON([]string{namespaceID, providerEventID, operationID, payloadHash}))
	return "gateway_webhook_event_collision_" + fmt.Sprintf("%x", sum[:16])
}

type gatewayOperationRow interface {
	Scan(...any) error
}

func scanGatewayOperation(row gatewayOperationRow) (Operation, error) {
	var op Operation
	err := row.Scan(
		&op.OperationID, &op.AttemptID, &op.ProviderConnectionID, &op.ProviderConnectionRevision,
		&op.ProviderNamespaceID, &op.Provider,
		&op.ProviderIdempotencyKey, &op.RequestHash, &op.RequestHashVersion, &op.Status,
		&op.SideEffectCertainty, &op.ProviderReference, &op.FailureCode, &op.FirstDispatchedAt, &op.CreatedAt, &op.UpdatedAt,
	)
	return op, err
}

func validateCurrentRequestHashes(op Operation) error {
	if op.ProviderConnectionID == "" || op.ProviderConnectionRevision == "" || op.ProviderNamespaceID == "" || op.Provider == "" ||
		op.RequestHashVersion != requestHashVersionConnection || op.RequestHash == "" || op.LegacyRequestHash == "" {
		return errors.New("current and legacy request hashes with an explicit current version are required")
	}
	if (op.Status == StatusCreated && op.FirstDispatchedAt != nil) ||
		(op.Status != StatusCreated && op.Status != StatusReview && op.FirstDispatchedAt == nil) {
		return errors.New("new provider operation dispatch history is inconsistent")
	}
	return nil
}

func (s *PostgresStore) ApplyWebhook(ctx context.Context, event VerifiedWebhookEvent) (ProviderOutcome, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	defer tx.Rollback(ctx)
	operation, err := scanGatewayOperation(tx.QueryRow(ctx, `
		SELECT operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		       provider, provider_idempotency_key, request_hash, request_hash_version, status,
		       side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at
		FROM provider_gateway.operations WHERE operation_id=$1 FOR UPDATE`, event.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderOutcome{}, false, ErrNotFound
	}
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	if !webhookEventMatchesOperationIdentity(event, operation) {
		return ProviderOutcome{}, false, ErrNotFound
	}
	outcome := ProviderOutcome{
		EventID: event.EventID, OperationID: event.OperationID,
		ProviderConnectionID: operation.ProviderConnectionID, ProviderConnectionRevision: operation.ProviderConnectionRevision,
		ProviderNamespaceID: operation.ProviderNamespaceID, Provider: operation.Provider,
		Status: event.Status, SideEffectCertainty: event.SideEffectCertainty,
		ProviderReference: event.ProviderReference, FailureCode: event.FailureCode, OccurredAt: event.OccurredAt,
	}
	rawOutcome := outcome
	observation, err := bindProviderObservation(operation, webhookProviderObservation(event, rawOutcome))
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	if _, err := insertProviderObservationPostgres(ctx, tx, observation); err != nil {
		return ProviderOutcome{}, false, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.webhook_events
		(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,sanitized_payload,provider_payload_hash)
		VALUES($1,$2,$3,$4,$5,$6,$7,sha256(convert_to(($8::jsonb)::text,'UTF8')))
		ON CONFLICT(provider_namespace_id,event_id) DO NOTHING`,
		outcome.ProviderConnectionID, outcome.ProviderConnectionRevision, outcome.ProviderNamespaceID,
		outcome.Provider, outcome.EventID, outcome.OperationID, mustJSON(rawOutcome), observation.SanitizedPayload)
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	if tag.RowsAffected() == 0 {
		var storedOperationID string
		var samePayload bool
		if err := tx.QueryRow(ctx, `
			SELECT operation_id,
			       sanitized_payload = $3::jsonb
			       AND (provider_payload_hash IS NULL OR provider_payload_hash = sha256(convert_to(($4::jsonb)::text,'UTF8')))
			FROM provider_gateway.webhook_events
			WHERE provider_namespace_id=$1 AND event_id=$2`,
			outcome.ProviderNamespaceID, outcome.EventID, mustJSON(rawOutcome), observation.SanitizedPayload).Scan(&storedOperationID, &samePayload); err != nil {
			return ProviderOutcome{}, false, err
		}
		if storedOperationID != outcome.OperationID || !samePayload {
			payloadHash := observation.PayloadHash
			if _, err := tx.Exec(ctx, `
				INSERT INTO provider_gateway.webhook_event_conflicts
				(provider_namespace_id,event_id,operation_id,path_connection_id,path_connection_revision,payload_hash,sanitized_payload)
				VALUES($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT(provider_namespace_id,event_id,payload_hash) DO NOTHING`,
				outcome.ProviderNamespaceID, outcome.EventID, outcome.OperationID,
				event.PathConnectionID, event.PathIdentity.ConnectionRevision, payloadHash, mustJSON(rawOutcome)); err != nil {
				return ProviderOutcome{}, false, err
			}
			operationIDs := []string{storedOperationID}
			if outcome.OperationID != storedOperationID {
				operationIDs = append(operationIDs, outcome.OperationID)
			}
			for _, operationID := range operationIDs {
				conflicted, err := markWebhookEventCollision(ctx, tx, operationID)
				if err != nil {
					return ProviderOutcome{}, false, err
				}
				if err := enqueueWebhookEventCollisionOutbox(ctx, tx, conflicted, outcome.EventID, payloadHash); err != nil {
					return ProviderOutcome{}, false, err
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return ProviderOutcome{}, false, err
			}
			return ProviderOutcome{}, false, ErrWebhookEventConflict
		}
		return outcome, false, tx.Commit(ctx)
	}
	preserveLease, err := preserveActiveLeaseForWebhook(ctx, tx, operation, outcome)
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	var collisionOwner *Operation
	if !preserveLease {
		updated, owner, _, err := applyStatusPostgres(
			ctx, tx, outcome.OperationID, outcome.Status, outcome.SideEffectCertainty,
			outcome.ProviderReference, outcome.FailureCode, nil,
		)
		if err != nil {
			return ProviderOutcome{}, false, err
		}
		collisionOwner = owner
		outcome.Status = updated.Status
		outcome.SideEffectCertainty = updated.SideEffectCertainty
		outcome.ProviderReference = updated.ProviderReference
		outcome.FailureCode = updated.FailureCode
	}
	if collisionOwner != nil {
		if err := enqueueReferenceCollisionOutbox(ctx, tx, *collisionOwner, outcome.OperationID); err != nil {
			return ProviderOutcome{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.outbox
		(provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7)`,
		outcome.ProviderConnectionID, outcome.ProviderConnectionRevision, outcome.ProviderNamespaceID,
		outcome.Provider, outcome.EventID, outcome.OperationID, mustJSON(outcome)); err != nil {
		return ProviderOutcome{}, false, err
	}
	return outcome, true, tx.Commit(ctx)
}

func preserveActiveLeaseForWebhook(ctx context.Context, tx pgx.Tx, operation Operation, outcome ProviderOutcome) (bool, error) {
	if outcome.Status != StatusUnknown {
		return false, nil
	}
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(dispatch_lease_until > clock_timestamp(),false)
		FROM provider_gateway.operations WHERE operation_id=$1`, operation.OperationID).Scan(&active); err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	if outcome.ProviderReference == "" {
		return true, nil
	}
	ownerID, err := recordProviderReferencePostgres(ctx, tx, operation, outcome.ProviderReference)
	if err != nil {
		return false, err
	}
	if ownerID != operation.OperationID ||
		(operation.ProviderReference != "" && operation.ProviderReference != outcome.ProviderReference) {
		return false, nil
	}
	return true, nil
}

func (s *PostgresStore) PendingOutbox(ctx context.Context, limit int) ([]OutboxItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,provider_connection_id,provider_connection_revision,provider_namespace_id,provider,event_id,operation_id,payload
		FROM provider_gateway.outbox
		WHERE delivered_at IS NULL
		ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []OutboxItem
	for rows.Next() {
		var item OutboxItem
		var connectionID, revision, namespaceID, provider, eventID, operationID string
		var payload []byte
		if err := rows.Scan(&item.ID, &connectionID, &revision, &namespaceID, &provider, &eventID, &operationID, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &item.Outcome); err != nil {
			return nil, err
		}
		if err := reconcileOutboxIdentity(&item.Outcome, connectionID, revision, namespaceID, provider, eventID, operationID); err != nil {
			return nil, fmt.Errorf("outbox item %d identity: %w", item.ID, err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func reconcileOutboxIdentity(outcome *ProviderOutcome, connectionID, revision, namespaceID, provider, eventID, operationID string) error {
	identities := []struct {
		name       string
		payload    *string
		relational string
	}{
		{name: "provider_connection_id", payload: &outcome.ProviderConnectionID, relational: connectionID},
		{name: "provider_connection_revision", payload: &outcome.ProviderConnectionRevision, relational: revision},
		{name: "provider_namespace_id", payload: &outcome.ProviderNamespaceID, relational: namespaceID},
		{name: "provider", payload: &outcome.Provider, relational: provider},
		{name: "event_id", payload: &outcome.EventID, relational: eventID},
		{name: "operation_id", payload: &outcome.OperationID, relational: operationID},
	}
	for _, identity := range identities {
		if *identity.payload == "" {
			*identity.payload = identity.relational
			continue
		}
		if *identity.payload != identity.relational {
			return fmt.Errorf("%s payload=%q relational=%q", identity.name, *identity.payload, identity.relational)
		}
	}
	return nil
}

func (s *PostgresStore) MarkOutboxDelivered(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE provider_gateway.outbox SET delivered_at=now() WHERE id=$1`, id)
	return err
}

type providerEventKey struct {
	NamespaceID string
	EventID     string
}

type providerConnectionRevisionKey struct {
	ConnectionID string
	Revision     string
}

type providerWebhookConflictKey struct {
	Event       providerEventKey
	PayloadHash string
}

type providerReferenceKey struct {
	NamespaceID string
	Reference   string
}

type providerReferenceObservationKey struct {
	Reference   providerReferenceKey
	OperationID string
}

type MemoryStore struct {
	mu                    sync.Mutex
	operations            map[string]Operation
	dispatchLeases        map[string]time.Time
	connectionRevisions   map[providerConnectionRevisionKey]ProviderConnectionRevisionBinding
	events                map[providerEventKey]ProviderOutcome
	eventPayloadHashes    map[providerEventKey]string
	referenceClaims       map[providerReferenceKey]string
	referenceObservations map[providerReferenceObservationKey]struct{}
	webhookConflicts      map[providerWebhookConflictKey]ProviderOutcome
	providerObservations  []ProviderOperationObservation
	outbox                []OutboxItem
	nextID                int64
	nextObservationID     int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		operations: map[string]Operation{}, events: map[providerEventKey]ProviderOutcome{},
		eventPayloadHashes: map[providerEventKey]string{}, referenceClaims: map[providerReferenceKey]string{},
		dispatchLeases:        map[string]time.Time{},
		connectionRevisions:   map[providerConnectionRevisionKey]ProviderConnectionRevisionBinding{},
		referenceObservations: map[providerReferenceObservationKey]struct{}{}, webhookConflicts: map[providerWebhookConflictKey]ProviderOutcome{},
	}
}

func (s *MemoryStore) Ping(context.Context) error { return nil }
func (s *MemoryStore) BindConnectionRevisions(_ context.Context, configured map[string]ProviderConnectionRevisionBinding) error {
	connectionIDs, err := orderedConnectionRevisionIDs(configured)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connectionID := range connectionIDs {
		binding := configured[connectionID]
		key := providerConnectionRevisionKey{ConnectionID: connectionID, Revision: binding.ConnectionRevision}
		if stored, exists := s.connectionRevisions[key]; exists && stored != binding {
			return fmt.Errorf("%w: connection=%q revision=%q", ErrConnectionRevisionConflict, connectionID, binding.ConnectionRevision)
		}
	}
	for _, connectionID := range connectionIDs {
		binding := configured[connectionID]
		s.connectionRevisions[providerConnectionRevisionKey{ConnectionID: connectionID, Revision: binding.ConnectionRevision}] = binding
	}
	return nil
}

func (s *MemoryStore) ValidateNonterminalConnections(_ context.Context, configured map[string]ProviderConnectionIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range s.operations {
		if op.Status != StatusCreated && op.Status != StatusSending && op.Status != StatusUnknown {
			continue
		}
		configuredIdentity, ok := configured[op.ProviderConnectionID]
		if !ok || configuredIdentity.Provider != op.Provider ||
			configuredIdentity.ConnectionRevision != op.ProviderConnectionRevision ||
			configuredIdentity.NamespaceID != op.ProviderNamespaceID {
			return fmt.Errorf("%w: connection=%q durable_revision=%q durable_namespace=%q durable_provider=%q configured=%+v",
				ErrUnconfiguredConnection, op.ProviderConnectionID, op.ProviderConnectionRevision,
				op.ProviderNamespaceID, op.Provider, configuredIdentity)
		}
	}
	return nil
}
func (s *MemoryStore) CreateOrGet(_ context.Context, op Operation) (Operation, bool, error) {
	if err := validateCurrentRequestHashes(op); err != nil {
		return Operation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.operations[op.OperationID]; ok {
		if existing.RequestHashVersion == requestHashVersionLegacy &&
			existing.ProviderConnectionID == existing.Provider &&
			existing.ProviderConnectionRevision == legacyConnectionRevision &&
			existing.ProviderNamespaceID == existing.Provider &&
			existing.AttemptID == op.AttemptID &&
			existing.ProviderConnectionID == op.ProviderConnectionID &&
			existing.ProviderConnectionRevision == op.ProviderConnectionRevision &&
			existing.ProviderNamespaceID == op.ProviderNamespaceID &&
			existing.Provider == op.Provider &&
			existing.ProviderIdempotencyKey == op.ProviderIdempotencyKey &&
			existing.RequestHash == op.LegacyRequestHash {
			existing.RequestHash = op.RequestHash
			existing.RequestHashVersion = requestHashVersionConnection
			s.operations[op.OperationID] = existing
			return existing, false, nil
		}
		if existing.RequestHashVersion != requestHashVersionConnection ||
			existing.RequestHash != op.RequestHash ||
			existing.AttemptID != op.AttemptID ||
			existing.ProviderConnectionID != op.ProviderConnectionID ||
			existing.ProviderConnectionRevision != op.ProviderConnectionRevision ||
			existing.ProviderNamespaceID != op.ProviderNamespaceID ||
			existing.Provider != op.Provider ||
			existing.ProviderIdempotencyKey != op.ProviderIdempotencyKey {
			return Operation{}, false, ErrIdempotencyConflict
		}
		return existing, false, nil
	}
	op.LegacyRequestHash = ""
	s.operations[op.OperationID] = op
	return op, true, nil
}
func (s *MemoryStore) Get(_ context.Context, id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return op, nil
}
func (s *MemoryStore) ClaimDispatch(_ context.Context, expected Operation, lease time.Duration) (Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.operations[expected.OperationID]
	if !ok {
		return Operation{}, false, ErrNotFound
	}
	if !dispatchClaimMatches(current, expected) ||
		(current.Status != StatusCreated && current.Status != StatusSending && current.Status != StatusUnknown) {
		return current, false, nil
	}
	now := time.Now().UTC()
	if deadline, active := s.dispatchLeases[current.OperationID]; active && now.Before(deadline) {
		return current, false, nil
	}
	if lease <= 0 {
		return current, false, errors.New("dispatch lease must be positive")
	}
	claimAt := now
	if !claimAt.After(current.UpdatedAt) {
		claimAt = current.UpdatedAt.Add(time.Nanosecond)
	}
	current.Status = StatusSending
	current.SideEffectCertainty = "possible"
	if current.FirstDispatchedAt == nil {
		first := claimAt
		current.FirstDispatchedAt = &first
	}
	current.UpdatedAt = claimAt
	s.operations[current.OperationID] = current
	s.dispatchLeases[current.OperationID] = now.Add(lease)
	return current, true, nil
}

func (s *MemoryStore) DispatchInFlight(_ context.Context, operationID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.operations[operationID]; !exists {
		return false, ErrNotFound
	}
	deadline, exists := s.dispatchLeases[operationID]
	return exists && time.Now().UTC().Before(deadline), nil
}

func dispatchClaimMatches(current, expected Operation) bool {
	return current.OperationID == expected.OperationID && current.AttemptID == expected.AttemptID &&
		current.ProviderConnectionID == expected.ProviderConnectionID &&
		current.ProviderConnectionRevision == expected.ProviderConnectionRevision &&
		current.ProviderNamespaceID == expected.ProviderNamespaceID && current.Provider == expected.Provider &&
		current.ProviderIdempotencyKey == expected.ProviderIdempotencyKey && current.RequestHash == expected.RequestHash &&
		current.RequestHashVersion == expected.RequestHashVersion && current.Status == expected.Status &&
		current.UpdatedAt.Equal(expected.UpdatedAt)
}
func (s *MemoryStore) SetStatus(_ context.Context, id, status, certainty, reference, failure string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, collisionOwner, err := s.applyStatusLocked(id, status, certainty, reference, failure)
	if err != nil {
		return Operation{}, err
	}
	if collisionOwner != nil {
		s.enqueueReferenceCollisionLocked(*collisionOwner, op.OperationID)
	}
	return op, nil
}

func (s *MemoryStore) SetStatusIfCurrent(_ context.Context, expected Operation, status, certainty, reference, failure string) (Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.operations[expected.OperationID]
	if !ok {
		return Operation{}, false, ErrNotFound
	}
	if !dispatchClaimMatches(current, expected) {
		return current, false, nil
	}
	op, collisionOwner, err := s.applyStatusLocked(expected.OperationID, status, certainty, reference, failure)
	if err != nil {
		return Operation{}, false, err
	}
	if collisionOwner != nil {
		s.enqueueReferenceCollisionLocked(*collisionOwner, op.OperationID)
	}
	return op, true, nil
}

func (s *MemoryStore) applyStatusLocked(id, status, certainty, reference, failure string) (Operation, *Operation, error) {
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, nil, ErrNotFound
	}
	delete(s.dispatchLeases, id)
	if reference != "" {
		claimKey := providerReferenceKey{NamespaceID: op.ProviderNamespaceID, Reference: reference}
		s.referenceObservations[providerReferenceObservationKey{Reference: claimKey, OperationID: op.OperationID}] = struct{}{}
		ownerID, claimed := s.referenceClaims[claimKey]
		if !claimed {
			s.referenceClaims[claimKey] = op.OperationID
			ownerID = op.OperationID
		}
		if ownerID != op.OperationID {
			owner, ownerExists := s.operations[ownerID]
			if !ownerExists {
				return Operation{}, nil, errors.New("provider reference claim owner is missing")
			}
			owner = markMemoryProviderReferenceCollision(owner, reference)
			op = markMemoryProviderReferenceCollision(op, reference)
			s.operations[ownerID] = owner
			s.operations[id] = op
			delete(s.dispatchLeases, ownerID)
			return op, &owner, nil
		}
	}
	op = applyOperationStatus(op, status, certainty, reference, failure)
	s.operations[id] = op
	return op, nil, nil
}

func markMemoryProviderReferenceCollision(op Operation, reference string) Operation {
	now := nextMemoryOperationTime(op)
	if op.Status != StatusReview || op.FailureCode == "" {
		op.FailureCode = "provider_reference_collision"
	}
	op.Status = StatusReview
	op.SideEffectCertainty = "possible"
	if op.ProviderReference == "" {
		op.ProviderReference = reference
	}
	op.UpdatedAt = now
	return op
}

func (s *MemoryStore) enqueueReferenceCollisionLocked(owner Operation, collidingOperationID string) {
	s.nextID++
	s.outbox = append(s.outbox, OutboxItem{ID: s.nextID, Outcome: ProviderOutcome{
		EventID:     collisionEventID(owner.ProviderNamespaceID, owner.ProviderReference, owner.OperationID, collidingOperationID),
		OperationID: owner.OperationID, ProviderConnectionID: owner.ProviderConnectionID,
		ProviderConnectionRevision: owner.ProviderConnectionRevision, ProviderNamespaceID: owner.ProviderNamespaceID,
		Provider: owner.Provider, Status: StatusReview, SideEffectCertainty: "possible",
		ProviderReference: owner.ProviderReference, FailureCode: "provider_reference_collision", OccurredAt: time.Now().UTC(),
	}})
}

func applyOperationStatus(op Operation, status, certainty, reference, failure string) Operation {
	now := nextMemoryOperationTime(op)
	if op.FirstDispatchedAt == nil && statusRecordsDispatch(status) {
		op.FirstDispatchedAt = &now
	}
	alreadyReview := op.Status == StatusReview
	referenceConflict := op.ProviderReference != "" && reference != "" && op.ProviderReference != reference
	authorizationFailureConflict :=
		(op.Status == StatusFailed && status == StatusUnknown && (reference != "" || failure == "authorization_only")) ||
			(status == StatusFailed && (op.Status == StatusCreated || op.Status == StatusSending || op.Status == StatusUnknown) &&
				(op.ProviderReference != "" || op.FailureCode == "authorization_only"))
	if alreadyReview {
		// Conflicting terminal evidence has already escalated the operation.
	} else if op.Status != StatusCreated && status == StatusCreated {
		// Dispatch history is monotonic; a late or invalid command cannot recreate
		// the Gateway's never-dispatched state.
	} else if op.Status == StatusCreated &&
		(status == StatusUnknown || status == StatusSucceeded || status == StatusFailed) {
		op.Status = StatusReview
		op.SideEffectCertainty = "possible"
	} else if referenceConflict || authorizationFailureConflict {
		op.Status = StatusReview
		op.SideEffectCertainty = "possible"
	} else if (op.Status == StatusSucceeded || op.Status == StatusFailed) &&
		(status == StatusCreated || status == StatusSending || status == StatusUnknown) {
		// Less-certain transport evidence must never downgrade a conclusive result.
	} else if (op.Status == StatusSucceeded || op.Status == StatusFailed) && op.Status != status {
		op.Status = StatusReview
		op.SideEffectCertainty = "possible"
	} else {
		op.Status = status
		op.SideEffectCertainty = certainty
	}
	if reference != "" && !referenceConflict {
		op.ProviderReference = reference
	}
	if alreadyReview {
		// The first durable quarantine reason remains the operator-facing cause.
	} else if referenceConflict {
		op.FailureCode = "provider_reference_conflict"
	} else if authorizationFailureConflict {
		op.FailureCode = "authorization_failure_conflict"
	} else if op.Status == StatusReview && op.FirstDispatchedAt == nil &&
		(status == StatusUnknown || status == StatusSucceeded || status == StatusFailed) {
		op.FailureCode = "provider_evidence_without_dispatch_record"
	} else if op.Status == StatusSucceeded {
		op.FailureCode = ""
	} else if failure != "" {
		op.FailureCode = failure
	}
	op.UpdatedAt = now
	return op
}

func nextMemoryOperationTime(op Operation) time.Time {
	now := time.Now().UTC()
	if !now.After(op.UpdatedAt) {
		now = op.UpdatedAt.Add(time.Nanosecond)
	}
	if now.Before(op.CreatedAt) {
		now = op.CreatedAt
	}
	return now
}

func statusRecordsDispatch(status string) bool {
	return status == StatusSending
}
func (s *MemoryStore) ApplyWebhook(_ context.Context, event VerifiedWebhookEvent) (ProviderOutcome, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[event.OperationID]
	if !ok {
		return ProviderOutcome{}, false, ErrNotFound
	}
	if !webhookEventMatchesOperationIdentity(event, op) {
		return ProviderOutcome{}, false, ErrNotFound
	}
	outcome := ProviderOutcome{
		EventID: event.EventID, OperationID: event.OperationID,
		ProviderConnectionID: op.ProviderConnectionID, ProviderConnectionRevision: op.ProviderConnectionRevision,
		ProviderNamespaceID: op.ProviderNamespaceID, Provider: op.Provider,
		Status: event.Status, SideEffectCertainty: event.SideEffectCertainty,
		ProviderReference: event.ProviderReference, FailureCode: event.FailureCode, OccurredAt: event.OccurredAt,
	}
	rawOutcome := outcome
	observation, err := bindProviderObservation(op, webhookProviderObservation(event, rawOutcome))
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	eventKey := providerEventKey{NamespaceID: outcome.ProviderNamespaceID, EventID: outcome.EventID}
	if stored, ok := s.events[eventKey]; ok {
		if string(mustJSON(stored)) != string(mustJSON(outcome)) || s.eventPayloadHashes[eventKey] != observation.PayloadHash {
			payloadHash := observation.PayloadHash
			conflictKey := providerWebhookConflictKey{Event: eventKey, PayloadHash: payloadHash}
			if _, alreadyStored := s.webhookConflicts[conflictKey]; !alreadyStored {
				s.webhookConflicts[conflictKey] = rawOutcome
				operationIDs := []string{stored.OperationID}
				if outcome.OperationID != stored.OperationID {
					operationIDs = append(operationIDs, outcome.OperationID)
				}
				for _, operationID := range operationIDs {
					conflicted, exists := s.operations[operationID]
					if !exists {
						return ProviderOutcome{}, false, ErrNotFound
					}
					if conflicted.Status != StatusReview || conflicted.FailureCode == "" {
						conflicted.FailureCode = "webhook_event_collision"
					}
					now := nextMemoryOperationTime(conflicted)
					conflicted.Status = StatusReview
					conflicted.SideEffectCertainty = "possible"
					conflicted.UpdatedAt = now
					s.operations[operationID] = conflicted
					s.enqueueWebhookEventCollisionLocked(conflicted, outcome.EventID, payloadHash)
				}
			}
			s.appendProviderObservationLocked(observation)
			return ProviderOutcome{}, false, ErrWebhookEventConflict
		}
		s.appendProviderObservationLocked(observation)
		return outcome, false, nil
	}
	preserveLease := s.preserveActiveLeaseForWebhookLocked(op, outcome)
	var collisionOwner *Operation
	if !preserveLease {
		var err error
		op, collisionOwner, err = s.applyStatusLocked(
			outcome.OperationID, outcome.Status, outcome.SideEffectCertainty,
			outcome.ProviderReference, outcome.FailureCode,
		)
		if err != nil {
			return ProviderOutcome{}, false, err
		}
		outcome.Status = op.Status
		outcome.SideEffectCertainty = op.SideEffectCertainty
		outcome.ProviderReference = op.ProviderReference
		outcome.FailureCode = op.FailureCode
	}
	s.events[eventKey] = rawOutcome
	s.eventPayloadHashes[eventKey] = observation.PayloadHash
	s.appendProviderObservationLocked(observation)
	if collisionOwner != nil {
		s.enqueueReferenceCollisionLocked(*collisionOwner, outcome.OperationID)
	}
	s.nextID++
	s.outbox = append(s.outbox, OutboxItem{ID: s.nextID, Outcome: outcome})
	return outcome, true, nil
}

func (s *MemoryStore) preserveActiveLeaseForWebhookLocked(operation Operation, outcome ProviderOutcome) bool {
	if outcome.Status != StatusUnknown {
		return false
	}
	deadline, exists := s.dispatchLeases[operation.OperationID]
	if !exists || !time.Now().UTC().Before(deadline) {
		return false
	}
	if outcome.ProviderReference == "" {
		return true
	}
	claimKey := providerReferenceKey{NamespaceID: operation.ProviderNamespaceID, Reference: outcome.ProviderReference}
	s.referenceObservations[providerReferenceObservationKey{Reference: claimKey, OperationID: operation.OperationID}] = struct{}{}
	ownerID, claimed := s.referenceClaims[claimKey]
	if !claimed {
		s.referenceClaims[claimKey] = operation.OperationID
		ownerID = operation.OperationID
	}
	return ownerID == operation.OperationID &&
		(operation.ProviderReference == "" || operation.ProviderReference == outcome.ProviderReference)
}

func (s *MemoryStore) enqueueWebhookEventCollisionLocked(operation Operation, providerEventID, payloadHash string) {
	s.nextID++
	s.outbox = append(s.outbox, OutboxItem{ID: s.nextID, Outcome: ProviderOutcome{
		EventID:     webhookCollisionEventID(operation.ProviderNamespaceID, providerEventID, operation.OperationID, payloadHash),
		OperationID: operation.OperationID, ProviderConnectionID: operation.ProviderConnectionID,
		ProviderConnectionRevision: operation.ProviderConnectionRevision, ProviderNamespaceID: operation.ProviderNamespaceID,
		Provider: operation.Provider, Status: StatusReview, SideEffectCertainty: "possible",
		ProviderReference: operation.ProviderReference, FailureCode: "webhook_event_collision", OccurredAt: time.Now().UTC(),
	}})
}
func (s *MemoryStore) PendingOutbox(_ context.Context, limit int) ([]OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit > len(s.outbox) {
		limit = len(s.outbox)
	}
	return append([]OutboxItem(nil), s.outbox[:limit]...), nil
}
func (s *MemoryStore) MarkOutboxDelivered(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.outbox {
		if s.outbox[i].ID == id {
			s.outbox = append(s.outbox[:i], s.outbox[i+1:]...)
			break
		}
	}
	return nil
}

func mustJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

func webhookEventMatchesOperationIdentity(event VerifiedWebhookEvent, op Operation) bool {
	if event.PathConnectionID == "" || event.PathIdentity.Provider == "" ||
		event.PathIdentity.ConnectionRevision == "" || event.PathIdentity.NamespaceID == "" {
		return false
	}
	pathIdentity, pathAllowed := event.AllowedOperationIdentities[event.PathConnectionID]
	if !pathAllowed || pathIdentity != event.PathIdentity ||
		event.PathIdentity.Provider != op.Provider || event.PathIdentity.NamespaceID != op.ProviderNamespaceID {
		return false
	}
	allowed, ok := event.AllowedOperationIdentities[op.ProviderConnectionID]
	return ok && allowed.Provider == op.Provider &&
		allowed.ConnectionRevision == op.ProviderConnectionRevision &&
		allowed.NamespaceID == op.ProviderNamespaceID
}
