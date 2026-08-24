package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("operation not found")
var ErrIdempotencyConflict = errors.New("operation id was reused with a different request")
var ErrWebhookEventConflict = errors.New("webhook event id was reused with different event data")

const gatewayMigrationLockID int64 = 710_130_613

type Store interface {
	CreateOrGet(context.Context, Operation) (Operation, bool, error)
	Get(context.Context, string) (Operation, error)
	SetStatus(context.Context, string, string, string, string, string) (Operation, error)
	ApplyWebhook(context.Context, ProviderOutcome) (ProviderOutcome, bool, error)
	PendingOutbox(context.Context, int) ([]OutboxItem, error)
	MarkOutboxDelivered(context.Context, int64) error
	Ping(context.Context) error
}

//go:embed migrations/0001_init.sql
var initialMigration string

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

	const version = "0001_init.sql"
	checksum := sha256.Sum256([]byte(initialMigration))
	var storedChecksum []byte
	err = connection.QueryRow(ctx,
		`SELECT checksum FROM provider_gateway.schema_migrations WHERE version=$1`, version,
	).Scan(&storedChecksum)
	if err == nil {
		if !bytes.Equal(storedChecksum, checksum[:]) {
			return fmt.Errorf("provider gateway migration %s checksum does not match the applied version", version)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check provider gateway migration %s: %w", version, err)
	}

	tx, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin provider gateway migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, initialMigration); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("apply provider gateway migration %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO provider_gateway.schema_migrations(version,checksum) VALUES($1,$2)`, version, checksum[:],
	); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("record provider gateway migration %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit provider gateway migration %s: %w", version, err)
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
	_, err := s.pool.Exec(ctx, `SELECT 1 FROM provider_gateway.operations LIMIT 0`)
	return err
}

func (s *PostgresStore) CreateOrGet(ctx context.Context, op Operation) (Operation, bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO provider_gateway.operations
		(operation_id, attempt_id, provider, provider_idempotency_key, request_hash, status, side_effect_certainty, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)
		ON CONFLICT (operation_id) DO NOTHING`,
		op.OperationID, op.AttemptID, op.Provider, op.ProviderIdempotencyKey, op.RequestHash, op.Status, op.SideEffectCertainty, op.CreatedAt)
	if err != nil {
		return Operation{}, false, err
	}
	if tag.RowsAffected() == 1 {
		return op, true, nil
	}
	existing, err := s.Get(ctx, op.OperationID)
	if err != nil {
		return Operation{}, false, err
	}
	if existing.RequestHash != op.RequestHash {
		return Operation{}, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Operation, error) {
	var op Operation
	err := s.pool.QueryRow(ctx, `
		SELECT operation_id, attempt_id, provider, provider_idempotency_key, request_hash, status,
		       side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), created_at, updated_at
		FROM provider_gateway.operations WHERE operation_id=$1`, id).Scan(
		&op.OperationID, &op.AttemptID, &op.Provider, &op.ProviderIdempotencyKey, &op.RequestHash, &op.Status,
		&op.SideEffectCertainty, &op.ProviderReference, &op.FailureCode, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return op, err
}

func (s *PostgresStore) SetStatus(ctx context.Context, id, status, certainty, reference, failureCode string) (Operation, error) {
	var op Operation
	err := s.pool.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET status = CASE
		      WHEN status = 'review_required' THEN status
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN 'review_required'
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'review_required'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'review_required'
		      WHEN status IN ('succeeded','failed') AND $2 IN ('created','sending','unknown') THEN status
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN 'review_required'
		      ELSE $2 END,
		    side_effect_certainty = CASE
		      WHEN status = 'review_required' THEN side_effect_certainty
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
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN failure_code
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'authorization_failure_conflict'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'authorization_failure_conflict'
		      WHEN status='succeeded' AND $2 IN ('created','sending','unknown') THEN NULL
		      WHEN status='failed' AND $2 IN ('created','sending','unknown') THEN failure_code
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN failure_code
		      WHEN $2='succeeded' THEN NULL
		      ELSE COALESCE(NULLIF($5,''),failure_code) END,
		    updated_at=now()
		WHERE operation_id=$1
		RETURNING operation_id, attempt_id, provider, provider_idempotency_key, request_hash, status,
		          side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), created_at, updated_at`,
		id, status, certainty, reference, failureCode).Scan(
		&op.OperationID, &op.AttemptID, &op.Provider, &op.ProviderIdempotencyKey, &op.RequestHash, &op.Status,
		&op.SideEffectCertainty, &op.ProviderReference, &op.FailureCode, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return op, err
}

func (s *PostgresStore) ApplyWebhook(ctx context.Context, outcome ProviderOutcome) (ProviderOutcome, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		INSERT INTO provider_gateway.webhook_events(provider, event_id, operation_id, sanitized_payload)
		VALUES($1,$2,$3,$4) ON CONFLICT(provider,event_id) DO NOTHING`,
		outcome.Provider, outcome.EventID, outcome.OperationID, mustJSON(outcome))
	if err != nil {
		return ProviderOutcome{}, false, err
	}
	if tag.RowsAffected() == 0 {
		var storedOperationID string
		var samePayload bool
		if err := tx.QueryRow(ctx, `
			SELECT operation_id, sanitized_payload = $3::jsonb
			FROM provider_gateway.webhook_events
			WHERE provider=$1 AND event_id=$2`,
			outcome.Provider, outcome.EventID, mustJSON(outcome)).Scan(&storedOperationID, &samePayload); err != nil {
			return ProviderOutcome{}, false, err
		}
		if storedOperationID != outcome.OperationID || !samePayload {
			return ProviderOutcome{}, false, ErrWebhookEventConflict
		}
		return outcome, false, tx.Commit(ctx)
	}
	err = tx.QueryRow(ctx, `
		UPDATE provider_gateway.operations
		SET status = CASE
		      WHEN status = 'review_required' THEN status
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN 'review_required'
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'review_required'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'review_required'
		      WHEN status IN ('succeeded','failed') AND $2 IN ('created','sending','unknown') THEN status
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN 'review_required'
		      ELSE $2 END,
		    side_effect_certainty=CASE
		      WHEN status = 'review_required' THEN side_effect_certainty
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
		      WHEN provider_reference IS NOT NULL AND $4 <> '' AND provider_reference <> $4 THEN failure_code
		      WHEN status='failed' AND $2='unknown' AND ($4 <> '' OR $5='authorization_only') THEN 'authorization_failure_conflict'
		      WHEN $2='failed' AND status IN ('created','sending','unknown') AND (provider_reference IS NOT NULL OR failure_code='authorization_only') THEN 'authorization_failure_conflict'
		      WHEN status='succeeded' AND $2 IN ('created','sending','unknown') THEN NULL
		      WHEN status='failed' AND $2 IN ('created','sending','unknown') THEN failure_code
		      WHEN status IN ('succeeded','failed') AND $2 IN ('succeeded','failed') AND status <> $2 THEN failure_code
		      WHEN $2='succeeded' THEN NULL
		      ELSE COALESCE(NULLIF($5,''),failure_code) END,
		    updated_at=now()
			WHERE operation_id=$1 AND provider=$6
			RETURNING status, side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,'')`,
		outcome.OperationID, outcome.Status, outcome.SideEffectCertainty, outcome.ProviderReference, outcome.FailureCode, outcome.Provider).Scan(
		&outcome.Status, &outcome.SideEffectCertainty, &outcome.ProviderReference, &outcome.FailureCode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProviderOutcome{}, false, ErrNotFound
		}
		return ProviderOutcome{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_gateway.outbox(provider, event_id, operation_id, payload) VALUES($1,$2,$3,$4)`, outcome.Provider, outcome.EventID, outcome.OperationID, mustJSON(outcome)); err != nil {
		return ProviderOutcome{}, false, err
	}
	return outcome, true, tx.Commit(ctx)
}

func (s *PostgresStore) PendingOutbox(ctx context.Context, limit int) ([]OutboxItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,payload FROM provider_gateway.outbox WHERE delivered_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []OutboxItem
	for rows.Next() {
		var item OutboxItem
		var payload []byte
		if err := rows.Scan(&item.ID, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &item.Outcome); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) MarkOutboxDelivered(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE provider_gateway.outbox SET delivered_at=now() WHERE id=$1`, id)
	return err
}

type MemoryStore struct {
	mu         sync.Mutex
	operations map[string]Operation
	events     map[string]ProviderOutcome
	outbox     []OutboxItem
	nextID     int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{operations: map[string]Operation{}, events: map[string]ProviderOutcome{}}
}

func (s *MemoryStore) Ping(context.Context) error { return nil }
func (s *MemoryStore) CreateOrGet(_ context.Context, op Operation) (Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.operations[op.OperationID]; ok {
		if existing.RequestHash != op.RequestHash {
			return Operation{}, false, ErrIdempotencyConflict
		}
		return existing, false, nil
	}
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
func (s *MemoryStore) SetStatus(_ context.Context, id, status, certainty, reference, failure string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	op = applyOperationStatus(op, status, certainty, reference, failure)
	s.operations[id] = op
	return op, nil
}

func applyOperationStatus(op Operation, status, certainty, reference, failure string) Operation {
	referenceConflict := op.ProviderReference != "" && reference != "" && op.ProviderReference != reference
	authorizationFailureConflict :=
		(op.Status == StatusFailed && status == StatusUnknown && (reference != "" || failure == "authorization_only")) ||
			(status == StatusFailed && (op.Status == StatusCreated || op.Status == StatusSending || op.Status == StatusUnknown) &&
				(op.ProviderReference != "" || op.FailureCode == "authorization_only"))
	if op.Status == StatusReview {
		// Conflicting terminal evidence has already escalated the operation.
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
	if authorizationFailureConflict {
		op.FailureCode = "authorization_failure_conflict"
	} else if op.Status == StatusSucceeded {
		op.FailureCode = ""
	} else if failure != "" {
		op.FailureCode = failure
	}
	op.UpdatedAt = time.Now().UTC()
	return op
}
func (s *MemoryStore) ApplyWebhook(_ context.Context, outcome ProviderOutcome) (ProviderOutcome, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rawOutcome := outcome
	eventKey := outcome.Provider + ":" + outcome.EventID
	if stored, ok := s.events[eventKey]; ok {
		if string(mustJSON(stored)) != string(mustJSON(outcome)) {
			return ProviderOutcome{}, false, ErrWebhookEventConflict
		}
		return outcome, false, nil
	}
	op, ok := s.operations[outcome.OperationID]
	if !ok {
		return ProviderOutcome{}, false, ErrNotFound
	}
	if op.Provider != outcome.Provider {
		return ProviderOutcome{}, false, ErrNotFound
	}
	op = applyOperationStatus(op, outcome.Status, outcome.SideEffectCertainty, outcome.ProviderReference, outcome.FailureCode)
	s.operations[outcome.OperationID] = op
	outcome.Status = op.Status
	outcome.SideEffectCertainty = op.SideEffectCertainty
	outcome.ProviderReference = op.ProviderReference
	outcome.FailureCode = op.FailureCode
	s.events[eventKey] = rawOutcome
	s.nextID++
	s.outbox = append(s.outbox, OutboxItem{ID: s.nextID, Outcome: outcome})
	return outcome, true, nil
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
