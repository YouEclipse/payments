package clients

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/recovery"
)

// GatewayRecoveryRequired is minted only after this client's authenticated
// Gateway response has passed schema, identity, payload and freshness checks.
// Its private fields cannot be populated by merchant or webhook JSON decoding.
type GatewayRecoveryRequired struct {
	evidence recovery.AbsenceEvidence
}

func (e *GatewayRecoveryRequired) Error() string {
	return "gateway supplied fresh same-operation absence evidence"
}
func (e *GatewayRecoveryRequired) Unwrap() error { return ErrGatewayOperationNotFound }

// EvidenceFor relabels the payload fingerprint only after the app has checked
// the original durable Payment command and computed the Gateway v2 hash from
// those exact bytes. All remaining identity fields must match byte-for-byte.
func (e *GatewayRecoveryRequired) EvidenceFor(original recovery.Identity) (recovery.AbsenceEvidence, error) {
	evidence := e.evidence
	expected := original
	expected.PayloadHash = evidence.Identity.PayloadHash
	if !original.Valid() || expected != evidence.Identity {
		return recovery.AbsenceEvidence{}, ErrGatewayIdentityMismatch
	}
	evidence.Identity.PayloadHash = original.PayloadHash
	return evidence, nil
}

type gatewayRecoveryProof struct {
	Version                    string               `json:"schema_version"`
	Kind                       recovery.AbsenceKind `json:"kind"`
	RequestHashVersion         int                  `json:"request_hash_version"`
	OperationID                string               `json:"operation_id"`
	AttemptID                  string               `json:"attempt_id"`
	ProviderConnectionID       string               `json:"provider_connection_id"`
	ProviderConnectionRevision string               `json:"provider_connection_revision"`
	ProviderNamespaceID        string               `json:"provider_namespace_id"`
	Provider                   string               `json:"provider"`
	ProviderIdempotencyKey     string               `json:"provider_idempotency_key"`
	RequestHash                string               `json:"request_hash"`
	ObservedAt                 time.Time            `json:"observed_at"`
	ValidUntil                 time.Time            `json:"valid_until"`
}

func validateGatewayRecovery(proof gatewayRecoveryProof, expected GatewayOperationIdentity, now time.Time) error {
	hashBytes, hashErr := hex.DecodeString(expected.RequestHash)
	identity := recovery.Identity{
		OperationID: expected.OperationID, AttemptID: expected.AttemptID,
		ConnectionID: expected.ProviderConnectionID, ConnectionRevision: expected.ProviderConnectionRevision,
		NamespaceID: expected.ProviderNamespaceID, Provider: expected.Provider,
		Key: expected.ProviderIdempotencyKey, PayloadHash: expected.RequestHash,
	}
	evidence := recovery.AbsenceEvidence{
		Kind: proof.Kind, ObservedAt: proof.ObservedAt, ValidUntil: proof.ValidUntil,
		Identity: recovery.Identity{
			OperationID: proof.OperationID, AttemptID: proof.AttemptID,
			ConnectionID: proof.ProviderConnectionID, ConnectionRevision: proof.ProviderConnectionRevision,
			NamespaceID: proof.ProviderNamespaceID, Provider: proof.Provider,
			Key: proof.ProviderIdempotencyKey, PayloadHash: proof.RequestHash,
		},
	}
	if proof.Version != "gateway_recovery.v1" || proof.RequestHashVersion != 2 ||
		proof.ValidUntil.Sub(proof.ObservedAt) > 5*time.Second ||
		hashErr != nil || len(hashBytes) != sha256.Size || hex.EncodeToString(hashBytes) != expected.RequestHash ||
		!evidence.ValidFor(identity, now) {
		return fmt.Errorf("%w: recovery evidence is stale, unsupported or does not bind the original command", ErrGatewayIdentityMismatch)
	}
	return &GatewayRecoveryRequired{evidence: evidence}
}

// GatewayRequestHashV2 mirrors the explicitly versioned Gateway wire contract.
// The payment command's own fingerprint remains separately persisted. This
// fixed struct intentionally includes empty Scenario; omitempty would change
// the hash and an unversioned map would hide a cross-service contract change.
func GatewayRequestHashV2(command GatewayOperationRequest) string {
	material := struct {
		Version                    int    `json:"version"`
		OperationID                string `json:"operation_id"`
		AttemptID                  string `json:"attempt_id"`
		ProviderConnectionID       string `json:"provider_connection_id"`
		ProviderConnectionRevision string `json:"provider_connection_revision"`
		ProviderNamespaceID        string `json:"provider_namespace_id"`
		Provider                   string `json:"provider"`
		AmountMinor                int64  `json:"amount_minor"`
		Currency                   string `json:"currency"`
		PaymentMethodToken         string `json:"payment_method_token"`
		CaptureMethod              string `json:"capture_method"`
		Scenario                   string `json:"scenario"`
	}{
		Version: 2, OperationID: command.OperationID, AttemptID: command.AttemptID,
		ProviderConnectionID: command.ProviderConnectionID, ProviderConnectionRevision: command.ProviderConnectionRevision,
		ProviderNamespaceID: command.ProviderNamespaceID, Provider: command.Provider,
		AmountMinor: command.AmountMinor, Currency: command.Currency, PaymentMethodToken: command.PaymentMethodToken,
		CaptureMethod: command.CaptureMethod, Scenario: command.Scenario,
	}
	encoded, _ := json.Marshal(material) // Fixed strings and integers cannot fail JSON encoding.
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
