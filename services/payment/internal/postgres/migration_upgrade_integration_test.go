//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/jackc/pgx/v5"
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
