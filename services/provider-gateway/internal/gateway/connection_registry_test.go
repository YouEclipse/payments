package gateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const registryTestConnectionID = "provider_registry_primary"

func TestConnectionRegistryRejectsMutationOfBoundRevision(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(Config)
	}{
		{"endpoint", func(cfg Config) { cfg.ProviderEndpoints[registryTestConnectionID] = "https://other.registry.test/api" }},
		{"webhook_secret", func(cfg Config) { cfg.WebhookSecrets[registryTestConnectionID] = "rotated-registry-test-secret" }},
		{"provider", func(cfg Config) { cfg.ProviderKinds[registryTestConnectionID] = "provider_registry_other" }},
		{"namespace", func(cfg Config) { cfg.ProviderNamespaceIDs[registryTestConnectionID] = "registry_other_account" }},
		{"conclusive_absence", func(cfg Config) { cfg.ProviderConclusiveAbsence[registryTestConnectionID] = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryStore()
			original := registryTestConfig("https://provider.registry.test/api", "registry-test-secret")
			if _, err := NewService(original, store, nil); err != nil {
				t.Fatalf("register original service: %v", err)
			}
			identity := registryTestIdentity("registry_v1")
			want := registryBindingForTest(t, identity, original.ProviderEndpoints[registryTestConnectionID], original.WebhookSecrets[registryTestConnectionID])
			drifted := registryTestConfig("https://provider.registry.test/api", "registry-test-secret")
			test.mutate(drifted)
			if _, err := NewService(drifted, store, nil); !errors.Is(err, ErrConnectionRevisionConflict) {
				t.Fatalf("changed %s error = %v, want ErrConnectionRevisionConflict", test.name, err)
			}
			if got, exists := registryMemoryBinding(store, registryTestConnectionID, identity.ConnectionRevision); !exists || got != want {
				t.Fatalf("conflicting startup overwrote binding: exists=%v got=%+v want=%+v", exists, got, want)
			}
			if _, err := NewService(original, store, nil); err != nil {
				t.Fatalf("exact original configuration no longer replays: %v", err)
			}
		})
	}
}

func TestConnectionRegistryNormalizedEndpointsHaveOneBinding(t *testing.T) {
	for _, test := range []struct {
		name       string
		normalized string
		variants   []string
	}{
		{"https", "https://provider.registry.test/api", []string{
			"https://PROVIDER.REGISTRY.TEST/api/",
			"HTTPS://Provider.Registry.Test:443/api///",
			"https://provider.registry.test/api",
		}},
		{"http", "http://provider.registry.test", []string{
			"http://PROVIDER.REGISTRY.TEST/",
			"HTTP://Provider.Registry.Test:80///",
			"http://provider.registry.test",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryStore()
			identity := registryTestIdentity("registry_v1")
			want := registryBindingForTest(t, identity, test.normalized, "registry-test-secret")
			for _, raw := range test.variants {
				normalized, err := normalizeProviderEndpoint(raw)
				if err != nil || normalized != test.normalized {
					t.Fatalf("normalizeProviderEndpoint(%q) = %q, %v; want %q", raw, normalized, err, test.normalized)
				}
				service, err := NewService(registryTestConfig(raw, "registry-test-secret"), store, nil)
				if err != nil {
					t.Fatalf("equivalent endpoint %q created a revision conflict: %v", raw, err)
				}
				if service.cfg.ProviderEndpoints[registryTestConnectionID] != test.normalized {
					t.Fatalf("execution endpoint differs from fingerprint endpoint: %q", service.cfg.ProviderEndpoints[registryTestConnectionID])
				}
				if got, exists := registryMemoryBinding(store, registryTestConnectionID, identity.ConnectionRevision); !exists || got != want {
					t.Fatalf("normalized endpoint changed binding: exists=%v got=%+v want=%+v", exists, got, want)
				}
			}
		})
	}
}

func TestProductionGatewayRejectsPlaintextExecutionBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"provider_endpoint", func(cfg *Config) { cfg.ProviderEndpoints[registryTestConnectionID] = "http://provider.registry.test" }},
		{"webhook_base", func(cfg *Config) { cfg.WebhookBaseURL = "http://gateway.registry.test" }},
		{"payment_outcome", func(cfg *Config) { cfg.PaymentOutcomeURL = "http://payment.registry.test/provider-outcomes" }},
		{"provider_credentials", func(cfg *Config) {
			cfg.ProviderEndpoints[registryTestConnectionID] = "https://user:secret@provider.registry.test"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := registryTestConfig("https://provider.registry.test", "registry-test-secret")
			cfg.Environment = "production"
			cfg.InternalToken = "production-internal-token"
			cfg.PaymentOutcomeURL = "https://payment.registry.test/provider-outcomes"
			test.mutate(&cfg)
			if _, err := NewService(cfg, NewMemoryStore(), nil); err == nil {
				t.Fatal("unsafe production URL was accepted")
			}
		})
	}
}

func TestMemoryConnectionRegistryBatchConflictIsAtomic(t *testing.T) {
	store := NewMemoryStore()
	identity := registryTestIdentity("registry_v1")
	original := registryBindingForTest(t, identity, "https://provider.registry.test", "registry-test-secret")
	conflict := registryBindingForTest(t, identity, "https://other.registry.test", "registry-test-secret")
	if err := store.BindConnectionRevisions(t.Context(), map[string]ProviderConnectionRevisionBinding{"z_existing": original}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindConnectionRevisions(t.Context(), map[string]ProviderConnectionRevisionBinding{
		"a_new": original, "z_existing": conflict,
	}); !errors.Is(err, ErrConnectionRevisionConflict) {
		t.Fatalf("conflicting batch error = %v, want ErrConnectionRevisionConflict", err)
	}
	if _, exists := registryMemoryBinding(store, "a_new", identity.ConnectionRevision); exists {
		t.Fatal("failed batch left a partially registered connection")
	}
	if got, exists := registryMemoryBinding(store, "z_existing", identity.ConnectionRevision); !exists || got != original {
		t.Fatalf("failed batch changed existing binding: exists=%v got=%+v", exists, got)
	}
}

func TestMemoryConnectionRegistryConcurrentConflictsKeepOneWinner(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	identity := registryTestIdentity("registry_v1")
	contenders := [2]ProviderConnectionRevisionBinding{
		registryBindingForTest(t, identity, "https://primary.registry.test", "registry-test-secret"),
		registryBindingForTest(t, identity, "https://backup.registry.test", "registry-test-secret"),
	}
	for iteration := range 16 {
		store := NewMemoryStore()
		connectionID := fmt.Sprintf("registry_race_%d", iteration)
		winner := registryConcurrentWinner(t, ctx, store, connectionID, contenders)
		if got, exists := registryMemoryBinding(store, connectionID, identity.ConnectionRevision); !exists || got != winner {
			t.Fatalf("concurrent loser overwrote winner: exists=%v got=%+v want=%+v", exists, got, winner)
		}
	}
}

func TestConnectionRegistryNewRevisionCannotRecoverOldUnknown(t *testing.T) {
	store := NewMemoryStore()
	cfg := registryTestConfig("https://provider.registry.test", "registry-test-secret")
	if _, err := NewService(cfg, store, nil); err != nil {
		t.Fatal(err)
	}
	originalIdentity := registryTestIdentity("registry_v1")
	op := registrySeedUnknownOperation(t, t.Context(), store, registryTestConnectionID, originalIdentity)
	drifted := registryTestConfig("https://next.registry.test", "registry-next-test-secret")
	drifted.ProviderConnectionRevisions[registryTestConnectionID] = "registry_v2"
	if _, err := NewService(drifted, store, nil); !errors.Is(err, ErrUnconfiguredConnection) {
		t.Fatalf("new revision recovered an old UNKNOWN operation: %v", err)
	}
	if _, exists := registryMemoryBinding(store, registryTestConnectionID, "registry_v2"); exists {
		t.Fatal("startup bound a new revision before rejecting its unresolved old operation")
	}
	got, err := store.Get(t.Context(), op.OperationID)
	if err != nil || got != op {
		t.Fatalf("rejected revision changed durable UNKNOWN operation: got=%+v err=%v", got, err)
	}
}

func TestConnectionRegistryReadinessRejectsPersistedBindingDrift(t *testing.T) {
	store := NewMemoryStore()
	service, err := NewService(registryTestConfig("https://provider.registry.test", "registry-test-secret"), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Ready(t.Context()); err != nil {
		t.Fatalf("initial readiness: %v", err)
	}
	identity := registryTestIdentity("registry_v1")
	drifted := registryBindingForTest(t, identity, "https://other.registry.test", "registry-test-secret")
	store.mu.Lock()
	store.connectionRevisions[providerConnectionRevisionKey{ConnectionID: registryTestConnectionID, Revision: identity.ConnectionRevision}] = drifted
	store.mu.Unlock()
	if err := service.Ready(t.Context()); !errors.Is(err, ErrConnectionRevisionConflict) {
		t.Fatalf("registry drift readiness error = %v, want ErrConnectionRevisionConflict", err)
	}
	if got, exists := registryMemoryBinding(store, registryTestConnectionID, identity.ConnectionRevision); !exists || got != drifted {
		t.Fatalf("readiness silently repaired conflicting registry: exists=%v got=%+v", exists, got)
	}
}

func registryTestIdentity(revision string) ProviderConnectionIdentity {
	return ProviderConnectionIdentity{Provider: "provider_registry", ConnectionRevision: revision, NamespaceID: "registry_account"}
}

func registryTestConfig(endpoint, secret string) Config {
	identity := registryTestIdentity("registry_v1")
	return Config{
		Environment:                 "test",
		ProviderKinds:               map[string]string{registryTestConnectionID: identity.Provider},
		ProviderConnectionRevisions: map[string]string{registryTestConnectionID: identity.ConnectionRevision},
		ProviderNamespaceIDs:        map[string]string{registryTestConnectionID: identity.NamespaceID},
		ProviderConclusiveAbsence:   map[string]bool{registryTestConnectionID: false},
		ProviderEndpoints:           map[string]string{registryTestConnectionID: endpoint},
		WebhookSecrets:              map[string]string{registryTestConnectionID: secret},
		WebhookBaseURL:              "https://gateway.registry.test",
	}
}

func registryBindingForTest(t *testing.T, identity ProviderConnectionIdentity, endpoint, secret string) ProviderConnectionRevisionBinding {
	t.Helper()
	normalized, err := normalizeProviderEndpoint(endpoint)
	if err != nil {
		t.Fatalf("normalize test endpoint: %v", err)
	}
	return connectionRevisionBinding(identity, normalized, secret, false)
}

func registryMemoryBinding(store *MemoryStore, connectionID, revision string) (ProviderConnectionRevisionBinding, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	binding, exists := store.connectionRevisions[providerConnectionRevisionKey{ConnectionID: connectionID, Revision: revision}]
	return binding, exists
}

func registryConcurrentWinner(t *testing.T, ctx context.Context, store Store, connectionID string, contenders [2]ProviderConnectionRevisionBinding) ProviderConnectionRevisionBinding {
	t.Helper()
	type result struct {
		index int
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, len(contenders))
	for index, binding := range contenders {
		go func() {
			<-start
			err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: binding})
			results <- result{index: index, err: err}
		}()
	}
	close(start)
	winner := -1
	conflicts := 0
	for range contenders {
		select {
		case result := <-results:
			if result.err == nil {
				if winner != -1 {
					t.Fatal("conflicting connection revision registrations both succeeded")
				}
				winner = result.index
			} else if errors.Is(result.err, ErrConnectionRevisionConflict) {
				conflicts++
			} else {
				t.Fatalf("concurrent registration failed unexpectedly: %v", result.err)
			}
		case <-ctx.Done():
			t.Fatalf("concurrent registry calls did not finish: %v", ctx.Err())
		}
	}
	if winner < 0 || conflicts != 1 {
		t.Fatalf("concurrent registration winner=%d conflicts=%d, want one of each", winner, conflicts)
	}
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: contenders[winner]}); err != nil {
		t.Fatalf("winner cannot replay its binding: %v", err)
	}
	if err := store.BindConnectionRevisions(ctx, map[string]ProviderConnectionRevisionBinding{connectionID: contenders[1-winner]}); !errors.Is(err, ErrConnectionRevisionConflict) {
		t.Fatalf("loser overwrote committed binding: %v", err)
	}
	return contenders[winner]
}

func registrySeedUnknownOperation(t *testing.T, ctx context.Context, store Store, connectionID string, identity ProviderConnectionIdentity) Operation {
	t.Helper()
	input := OperationRequest{
		OperationID: "op_" + connectionID, AttemptID: "att_" + connectionID,
		ProviderConnectionID: connectionID, ProviderConnectionRevision: identity.ConnectionRevision,
		ProviderNamespaceID: identity.NamespaceID, Provider: identity.Provider,
		AmountMinor: 100, Currency: "USD", PaymentMethodToken: "pm_registry_test", CaptureMethod: "automatic",
	}
	now := time.Now().UTC()
	op, inserted, err := store.CreateOrGet(ctx, Operation{
		OperationID: input.OperationID, AttemptID: input.AttemptID,
		ProviderConnectionID: connectionID, ProviderConnectionRevision: identity.ConnectionRevision,
		ProviderNamespaceID: identity.NamespaceID, Provider: identity.Provider,
		ProviderIdempotencyKey: input.OperationID, RequestHash: operationHashV2(input),
		RequestHashVersion: requestHashVersionConnection, LegacyRequestHash: operationHashV1(input),
		Status: StatusUnknown, SideEffectCertainty: "possible", FirstDispatchedAt: &now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !inserted {
		t.Fatalf("seed registry UNKNOWN operation: inserted=%v err=%v", inserted, err)
	}
	return op
}
