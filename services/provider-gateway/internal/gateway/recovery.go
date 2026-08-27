package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const recoveryEvidenceVersion = "gateway_recovery.v1"
const recoveryEvidenceFreshness = 5 * time.Second
const localRecoveryWindow = 24 * time.Hour

// These are explicit local/simulator adapter guarantees. A production adapter
// must provide its own conclusive-absence and idempotency-retention contract;
// neither a lease nor these constants can grant it permission to resubmit.
type RecoveryQuery struct {
	OperationID                string `json:"operation_id"`
	AttemptID                  string `json:"attempt_id"`
	ProviderConnectionID       string `json:"provider_connection_id"`
	ProviderConnectionRevision string `json:"provider_connection_revision"`
	ProviderNamespaceID        string `json:"provider_namespace_id"`
	Provider                   string `json:"provider"`
	ProviderIdempotencyKey     string `json:"provider_idempotency_key"`
	RequestHash                string `json:"request_hash"`
}

type RecoveryEvidence struct {
	Version            string `json:"schema_version"`
	Kind               string `json:"kind"`
	RequestHashVersion int    `json:"request_hash_version"`
	RecoveryQuery
	ObservedAt time.Time `json:"observed_at"`
	ValidUntil time.Time `json:"valid_until"`
}

// InspectRecovery performs no funds writes. Absence is a separate response
// from a provider outcome and is bound to the query's full original command.
// An absent Gateway row proves only that this Gateway has no dispatch record;
// it relies on the durable-before-dispatch invariant, not a provider HTTP 404.
func (s *Service) InspectRecovery(ctx context.Context, query RecoveryQuery) (Operation, *RecoveryEvidence, error) {
	if !validRecoveryQuery(query) {
		return Operation{}, nil, ErrInvalidOperation
	}
	configuredRequest := OperationRequest{
		ProviderConnectionID: query.ProviderConnectionID, ProviderConnectionRevision: query.ProviderConnectionRevision,
		ProviderNamespaceID: query.ProviderNamespaceID, Provider: query.Provider,
	}
	if err := s.validateConfiguredRequestIdentity(configuredRequest); err != nil {
		return Operation{}, nil, ErrUnconfiguredConnection
	}
	stored, err := s.store.Get(ctx, query.OperationID)
	if errors.Is(err, ErrNotFound) {
		if !IsDevelopmentEnvironment(s.cfg.Environment) {
			return Operation{}, nil, ErrNotFound
		}
		if _, recoveryOnly := s.cfg.ProviderRecoveryAliases[query.ProviderConnectionID]; recoveryOnly {
			return Operation{}, nil, ErrUnconfiguredConnection
		}
		now := time.Now().UTC()
		return Operation{}, newRecoveryEvidence(query, "gateway_operation_absent", now, now.Add(recoveryEvidenceFreshness)), nil
	}
	if err != nil {
		return Operation{}, nil, err
	}
	if stored.AttemptID != query.AttemptID || stored.ProviderConnectionID != query.ProviderConnectionID ||
		stored.ProviderConnectionRevision != query.ProviderConnectionRevision || stored.ProviderNamespaceID != query.ProviderNamespaceID ||
		stored.Provider != query.Provider || stored.ProviderIdempotencyKey != query.ProviderIdempotencyKey {
		return Operation{}, nil, ErrIdempotencyConflict
	}
	if stored.RequestHashVersion == requestHashVersionConnection && stored.RequestHash != query.RequestHash {
		return Operation{}, nil, ErrIdempotencyConflict
	}
	op, err := s.Get(ctx, query.OperationID, true)
	if !errors.Is(err, ErrResubmitRequired) {
		return op, nil, err
	}
	// A legacy v1 hash cannot be presented as proof of a v2 payload. Querying
	// legacy operations is still safe; resubmission needs explicit migration.
	if stored.RequestHashVersion != requestHashVersionConnection || !IsDevelopmentEnvironment(s.cfg.Environment) {
		return Operation{}, nil, ErrResubmitRequired
	}
	now := time.Now().UTC()
	deadline := stored.CreatedAt.Add(localRecoveryWindow)
	if !now.Before(deadline) {
		reviewed, storeErr := s.setStatusDurably(ctx, stored.OperationID, StatusReview, "possible", "", "local_recovery_window_exhausted")
		return reviewed, nil, storeErr
	}
	kind := "provider_conclusive_absent"
	if stored.Status == StatusCreated {
		kind = "gateway_never_dispatched"
	}
	validUntil := now.Add(recoveryEvidenceFreshness)
	if deadline.Before(validUntil) {
		validUntil = deadline
	}
	return stored, newRecoveryEvidence(query, kind, now, validUntil), nil
}

func newRecoveryEvidence(query RecoveryQuery, kind string, observed, until time.Time) *RecoveryEvidence {
	return &RecoveryEvidence{
		Version: recoveryEvidenceVersion, Kind: kind, RequestHashVersion: requestHashVersionConnection,
		RecoveryQuery: query, ObservedAt: observed, ValidUntil: until,
	}
}

func validRecoveryQuery(query RecoveryQuery) bool {
	for _, value := range []string{
		query.OperationID, query.AttemptID, query.ProviderConnectionID, query.ProviderConnectionRevision,
		query.ProviderNamespaceID, query.Provider, query.ProviderIdempotencyKey, query.RequestHash,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
			return false
		}
	}
	hash, err := hex.DecodeString(query.RequestHash)
	return err == nil && len(hash) == 32 && query.RequestHash == strings.ToLower(query.RequestHash) && query.ProviderIdempotencyKey == query.OperationID
}
