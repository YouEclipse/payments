//go:build integration

package gateway

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresConnectionRevisionRegistryRejectsDeleteAndTruncate(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	suffix := fmt.Sprint(time.Now().UnixNano())
	connectionID := "provider_registry_guard_" + suffix
	revision := "provider_registry_guard_v1_" + suffix
	binding := ProviderConnectionRevisionBinding{
		ProviderConnectionIdentity: ProviderConnectionIdentity{
			Provider: "provider_registry_guard", ConnectionRevision: revision,
			NamespaceID: "provider_registry_guard",
		},
		FingerprintVersion: executionFingerprintVersion,
		ConfigFingerprint:  fmt.Sprintf("%064x", time.Now().UnixNano()),
		ConclusiveAbsence:  true,
	}
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: binding}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
		DELETE FROM provider_gateway.provider_connection_revisions
		WHERE provider_connection_id=$1 AND provider_connection_revision=$2`, connectionID, revision); err == nil {
		t.Fatal("immutable connection revision DELETE succeeded")
	} else {
		requireGatewayCheckViolation(t, err)
	}
	if _, err := store.pool.Exec(ctx, `TRUNCATE provider_gateway.provider_connection_revisions`); err == nil {
		t.Fatal("immutable connection revision TRUNCATE succeeded")
	} else {
		requireGatewayCheckViolation(t, err)
	}
	var retained int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.provider_connection_revisions
		WHERE provider_connection_id=$1 AND provider_connection_revision=$2`, connectionID, revision).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("connection revision binding count=%d after rejected removal", retained)
	}
}
