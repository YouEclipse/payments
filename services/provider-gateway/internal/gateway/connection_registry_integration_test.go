//go:build integration

package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresConnectionRegistryRejectsMutationOfBoundRevision(t *testing.T) {
	store, ctx, connectionID := registryPostgresStore(t)
	identity := registryTestIdentity("registry_v1")
	original := registryBindingForTest(t, identity, "https://provider.registry.test", "registry-test-secret")
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: original}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: original}); err != nil {
		t.Fatalf("exact binding replay: %v", err)
	}
	providerDrift := identity
	providerDrift.Provider = "provider_registry_other"
	namespaceDrift := identity
	namespaceDrift.NamespaceID = "registry_other_account"
	for _, test := range []struct {
		name    string
		binding ProviderConnectionRevisionBinding
	}{
		{"endpoint", registryBindingForTest(t, identity, "https://other.registry.test", "registry-test-secret")},
		{"webhook_secret", registryBindingForTest(t, identity, "https://provider.registry.test", "registry-next-test-secret")},
		{"provider", registryBindingForTest(t, providerDrift, "https://provider.registry.test", "registry-test-secret")},
		{"namespace", registryBindingForTest(t, namespaceDrift, "https://provider.registry.test", "registry-test-secret")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: test.binding}); !errors.Is(err, ErrConnectionRevisionConflict) {
				t.Fatalf("changed %s error = %v, want ErrConnectionRevisionConflict", test.name, err)
			}
			if got := registryPostgresBinding(t, ctx, store, connectionID, identity.ConnectionRevision); got != original {
				t.Fatalf("rejected mutation overwrote original binding: got=%+v want=%+v", got, original)
			}
		})
	}
	newIdentity := identity
	newIdentity.ConnectionRevision = "registry_v2"
	newBinding := registryBindingForTest(t, newIdentity, "https://next.registry.test", "registry-next-test-secret")
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: newBinding}); err != nil {
		t.Fatalf("distinct new revision should be registerable: %v", err)
	}
	if got := registryPostgresBinding(t, ctx, store, connectionID, identity.ConnectionRevision); got != original {
		t.Fatalf("new revision changed historical binding: got=%+v want=%+v", got, original)
	}
	if got := registryPostgresBinding(t, ctx, store, connectionID, newIdentity.ConnectionRevision); got != newBinding {
		t.Fatalf("new revision binding: got=%+v want=%+v", got, newBinding)
	}
}

func TestPostgresConnectionRegistryBatchConflictRollsBack(t *testing.T) {
	store, ctx, prefix := registryPostgresStore(t)
	identity := registryTestIdentity("registry_v1")
	original := registryBindingForTest(t, identity, "https://provider.registry.test", "registry-test-secret")
	conflict := registryBindingForTest(t, identity, "https://other.registry.test", "registry-test-secret")
	// The new key sorts before the conflicting key, so rollback must undo an
	// INSERT that already ran, not merely reject a batch before its first write.
	newID, existingID := prefix+"_a_new", prefix+"_z_existing"
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{existingID: original}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{
		newID: original, existingID: conflict,
	}); !errors.Is(err, ErrConnectionRevisionConflict) {
		t.Fatalf("conflicting batch error = %v, want ErrConnectionRevisionConflict", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_gateway.provider_connection_revisions
		WHERE provider_connection_id=$1 AND provider_connection_revision=$2`, newID, identity.ConnectionRevision).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed batch left %d partially registered revisions", count)
	}
	if got := registryPostgresBinding(t, ctx, store, existingID, identity.ConnectionRevision); got != original {
		t.Fatalf("failed batch changed existing binding: got=%+v want=%+v", got, original)
	}
}

func TestPostgresConnectionRegistryConcurrentConflictsKeepOneWinner(t *testing.T) {
	store, ctx, prefix := registryPostgresStore(t)
	identity := registryTestIdentity("registry_v1")
	contenders := [2]ProviderConnectionRevisionBinding{
		registryBindingForTest(t, identity, "https://primary.registry.test", "registry-test-secret"),
		registryBindingForTest(t, identity, "https://backup.registry.test", "registry-test-secret"),
	}
	for iteration := range 4 {
		connectionID := fmt.Sprintf("%s_race_%d", prefix, iteration)
		winner := registryConcurrentWinner(t, ctx, store, connectionID, contenders)
		if got := registryPostgresBinding(t, ctx, store, connectionID, identity.ConnectionRevision); got != winner {
			t.Fatalf("concurrent loser overwrote winner: got=%+v want=%+v", got, winner)
		}
	}
}

func TestPostgresConnectionRegistryNewRevisionCannotRecoverOldUnknown(t *testing.T) {
	store, ctx, connectionID := registryPostgresStore(t)
	originalIdentity := registryTestIdentity("registry_v1")
	original := registryBindingForTest(t, originalIdentity, "https://provider.registry.test", "registry-test-secret")
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: original}); err != nil {
		t.Fatal(err)
	}
	op := registrySeedUnknownOperation(t, ctx, store, connectionID, originalIdentity)
	newIdentity := originalIdentity
	newIdentity.ConnectionRevision = "registry_v2"
	newBinding := registryBindingForTest(t, newIdentity, "https://next.registry.test", "registry-next-test-secret")
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: newBinding}); err != nil {
		t.Fatalf("register independently valid new revision: %v", err)
	}

	// Other tests intentionally retain nonterminal operations in this database.
	// Include all of them so the rejection must be caused by this test's unique
	// connection, not by assuming a private/empty provider_gateway schema.
	configured := registryNonterminalSnapshot(t, ctx, store)
	if err := store.ValidateNonterminalConnections(ctx, configured); err != nil {
		t.Fatalf("current nonterminal snapshot should resolve before drift: %v", err)
	}
	configured[connectionID] = newIdentity
	err := store.ValidateNonterminalConnections(ctx, configured)
	if !errors.Is(err, ErrUnconfiguredConnection) || !strings.Contains(err.Error(), connectionID) {
		t.Fatalf("new revision must reject this test's old UNKNOWN operation: %v", err)
	}
	got, err := store.Get(ctx, op.OperationID)
	if err != nil || got.Status != StatusUnknown || got.ProviderConnectionID != connectionID ||
		got.ProviderConnectionRevision != originalIdentity.ConnectionRevision || got.ProviderNamespaceID != originalIdentity.NamespaceID {
		t.Fatalf("new revision changed durable UNKNOWN identity: operation=%+v err=%v", got, err)
	}
	if got := registryPostgresBinding(t, ctx, store, connectionID, originalIdentity.ConnectionRevision); got != original {
		t.Fatalf("old UNKNOWN operation lost its original registry binding: got=%+v want=%+v", got, original)
	}
}

func registryPostgresStore(t *testing.T) (*PostgresStore, context.Context, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	store, err := OpenPostgres(ctx, databaseURL, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, ctx, fmt.Sprintf("registry_%s_%d", t.Name(), time.Now().UnixNano())
}

func registryPostgresBinding(t *testing.T, ctx context.Context, store *PostgresStore, connectionID, revision string) ProviderConnectionRevisionBinding {
	t.Helper()
	var binding ProviderConnectionRevisionBinding
	if err := store.pool.QueryRow(ctx, `
		SELECT provider,provider_connection_revision,provider_namespace_id,fingerprint_version,config_fingerprint
		FROM provider_gateway.provider_connection_revisions
		WHERE provider_connection_id=$1 AND provider_connection_revision=$2`, connectionID, revision).Scan(
		&binding.Provider, &binding.ConnectionRevision, &binding.NamespaceID, &binding.FingerprintVersion, &binding.ConfigFingerprint,
	); err != nil {
		t.Fatalf("read persisted registry binding: %v", err)
	}
	return binding
}

func registryNonterminalSnapshot(t *testing.T, ctx context.Context, store *PostgresStore) map[string]ProviderConnectionIdentity {
	t.Helper()
	rows, err := store.pool.Query(ctx, `
		SELECT DISTINCT provider_connection_id,provider_connection_revision,provider_namespace_id,provider
		FROM provider_gateway.operations WHERE status IN ('created','sending','unknown')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	configured := make(map[string]ProviderConnectionIdentity)
	for rows.Next() {
		var connectionID string
		var identity ProviderConnectionIdentity
		if err := rows.Scan(&connectionID, &identity.ConnectionRevision, &identity.NamespaceID, &identity.Provider); err != nil {
			t.Fatal(err)
		}
		if previous, exists := configured[connectionID]; exists && previous != identity {
			t.Fatalf("shared database already has incompatible nonterminal tuples for connection %q", connectionID)
		}
		configured[connectionID] = identity
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return configured
}
