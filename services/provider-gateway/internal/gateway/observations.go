package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ApplyProviderObservation appends the sanitized evidence and applies its
// normalized value through the operation reducer in one transaction. When
// expected is non-nil, a stale dispatch/query generation still leaves evidence
// in the ledger but cannot alter the newer operation state or lease.
func (s *PostgresStore) ApplyProviderObservation(
	ctx context.Context,
	observation ProviderOperationObservation,
	expected *Operation,
) (Operation, bool, error) {
	if expected != nil && expected.OperationID != observation.OperationID {
		return Operation{}, false, errors.New("provider observation generation belongs to another operation")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Operation{}, false, err
	}
	defer tx.Rollback(ctx)

	locked, err := lockProviderOperation(ctx, tx, observation.OperationID)
	if err != nil {
		return Operation{}, false, err
	}
	observation, err = bindProviderObservation(locked, observation)
	if err != nil {
		return Operation{}, false, err
	}
	if _, err := insertProviderObservationPostgres(ctx, tx, observation); err != nil {
		return Operation{}, false, err
	}
	if expected != nil && !dispatchClaimMatches(locked, *expected) {
		if err := tx.Commit(ctx); err != nil {
			return Operation{}, false, err
		}
		return locked, false, nil
	}

	updated, collisionOwner, applied, err := applyStatusPostgres(
		ctx,
		tx,
		observation.OperationID,
		observation.NormalizedStatus,
		observation.NormalizedCertainty,
		observation.NormalizedProviderReference,
		observation.NormalizedFailureCode,
		expected,
	)
	if err != nil {
		return Operation{}, false, err
	}
	if collisionOwner != nil {
		if err := enqueueReferenceCollisionOutbox(ctx, tx, *collisionOwner, updated.OperationID); err != nil {
			return Operation{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, false, err
	}
	return updated, applied, nil
}

func (s *PostgresStore) ListProviderObservations(ctx context.Context, operationID string) ([]ProviderOperationObservation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, operation_id, attempt_id, provider_connection_id, provider_connection_revision,
		       provider_namespace_id, provider, source, raw_provider_status, normalized_status,
		       normalized_certainty, COALESCE(normalized_provider_reference,''),
		       COALESCE(normalized_failure_code,''), COALESCE(provider_event_id,''),
		       encode(payload_hash,'hex'), sanitized_payload::text, observed_at, created_at
		FROM provider_gateway.provider_operation_observations
		WHERE operation_id=$1
		ORDER BY id`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []ProviderOperationObservation
	for rows.Next() {
		observation, err := scanProviderObservation(rows)
		if err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	return observations, rows.Err()
}

func lockProviderOperation(ctx context.Context, tx pgx.Tx, operationID string) (Operation, error) {
	op, err := scanGatewayOperation(tx.QueryRow(ctx, `
		SELECT operation_id, attempt_id, provider_connection_id, provider_connection_revision, provider_namespace_id,
		       provider, provider_idempotency_key, request_hash, request_hash_version, status,
		       side_effect_certainty, COALESCE(provider_reference,''), COALESCE(failure_code,''), first_dispatched_at, created_at, updated_at
		FROM provider_gateway.operations WHERE operation_id=$1 FOR UPDATE`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return op, err
}

func insertProviderObservationPostgres(ctx context.Context, tx pgx.Tx, observation ProviderOperationObservation) (ProviderOperationObservation, error) {
	var sanitizedPayload string
	err := tx.QueryRow(ctx, `
		INSERT INTO provider_gateway.provider_operation_observations (
			operation_id, attempt_id, provider_connection_id, provider_connection_revision,
			provider_namespace_id, provider, source, raw_provider_status, normalized_status,
			normalized_certainty, normalized_provider_reference, normalized_failure_code,
			provider_event_id, payload_hash, sanitized_payload, observed_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),
			sha256(convert_to(($14::jsonb)::text,'UTF8')),$14::jsonb,$15
		)
		RETURNING id, encode(payload_hash,'hex'), sanitized_payload::text, created_at`,
		observation.OperationID,
		observation.AttemptID,
		observation.ProviderConnectionID,
		observation.ProviderConnectionRevision,
		observation.ProviderNamespaceID,
		observation.Provider,
		observation.Source,
		observation.RawProviderStatus,
		observation.NormalizedStatus,
		observation.NormalizedCertainty,
		observation.NormalizedProviderReference,
		observation.NormalizedFailureCode,
		observation.ProviderEventID,
		[]byte(observation.SanitizedPayload),
		observation.ObservedAt,
	).Scan(&observation.ID, &observation.PayloadHash, &sanitizedPayload, &observation.CreatedAt)
	observation.SanitizedPayload = json.RawMessage(sanitizedPayload)
	return observation, err
}

type providerObservationRow interface {
	Scan(...any) error
}

func scanProviderObservation(row providerObservationRow) (ProviderOperationObservation, error) {
	var observation ProviderOperationObservation
	var sanitizedPayload string
	err := row.Scan(
		&observation.ID,
		&observation.OperationID,
		&observation.AttemptID,
		&observation.ProviderConnectionID,
		&observation.ProviderConnectionRevision,
		&observation.ProviderNamespaceID,
		&observation.Provider,
		&observation.Source,
		&observation.RawProviderStatus,
		&observation.NormalizedStatus,
		&observation.NormalizedCertainty,
		&observation.NormalizedProviderReference,
		&observation.NormalizedFailureCode,
		&observation.ProviderEventID,
		&observation.PayloadHash,
		&sanitizedPayload,
		&observation.ObservedAt,
		&observation.CreatedAt,
	)
	observation.SanitizedPayload = json.RawMessage(sanitizedPayload)
	return observation, err
}

func (s *MemoryStore) ApplyProviderObservation(
	_ context.Context,
	observation ProviderOperationObservation,
	expected *Operation,
) (Operation, bool, error) {
	if expected != nil && expected.OperationID != observation.OperationID {
		return Operation{}, false, errors.New("provider observation generation belongs to another operation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.operations[observation.OperationID]
	if !ok {
		return Operation{}, false, ErrNotFound
	}
	observation, err := bindProviderObservation(current, observation)
	if err != nil {
		return Operation{}, false, err
	}
	if expected != nil && !dispatchClaimMatches(current, *expected) {
		s.appendProviderObservationLocked(observation)
		return current, false, nil
	}
	updated, collisionOwner, err := s.applyStatusLocked(
		observation.OperationID,
		observation.NormalizedStatus,
		observation.NormalizedCertainty,
		observation.NormalizedProviderReference,
		observation.NormalizedFailureCode,
	)
	if err != nil {
		return Operation{}, false, err
	}
	s.appendProviderObservationLocked(observation)
	if collisionOwner != nil {
		s.enqueueReferenceCollisionLocked(*collisionOwner, updated.OperationID)
	}
	return updated, true, nil
}

func (s *MemoryStore) ListProviderObservations(_ context.Context, operationID string) ([]ProviderOperationObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.operations[operationID]; !ok {
		return nil, ErrNotFound
	}
	observations := make([]ProviderOperationObservation, 0)
	for _, observation := range s.providerObservations {
		if observation.OperationID != operationID {
			continue
		}
		copy := observation
		copy.SanitizedPayload = append(json.RawMessage(nil), observation.SanitizedPayload...)
		observations = append(observations, copy)
	}
	return observations, nil
}

func (s *MemoryStore) appendProviderObservationLocked(observation ProviderOperationObservation) {
	s.nextObservationID++
	observation.ID = s.nextObservationID
	observation.CreatedAt = time.Now().UTC()
	observation.SanitizedPayload = append(json.RawMessage(nil), observation.SanitizedPayload...)
	s.providerObservations = append(s.providerObservations, observation)
}

func bindProviderObservation(operation Operation, observation ProviderOperationObservation) (ProviderOperationObservation, error) {
	if operation.OperationID == "" || observation.OperationID != operation.OperationID {
		return ProviderOperationObservation{}, errors.New("provider observation operation identity is missing or inconsistent")
	}
	identity := []struct {
		name     string
		provided string
		durable  string
	}{
		{name: "attempt_id", provided: observation.AttemptID, durable: operation.AttemptID},
		{name: "provider_connection_id", provided: observation.ProviderConnectionID, durable: operation.ProviderConnectionID},
		{name: "provider_connection_revision", provided: observation.ProviderConnectionRevision, durable: operation.ProviderConnectionRevision},
		{name: "provider_namespace_id", provided: observation.ProviderNamespaceID, durable: operation.ProviderNamespaceID},
		{name: "provider", provided: observation.Provider, durable: operation.Provider},
	}
	for _, field := range identity {
		if field.provided != "" && field.provided != field.durable {
			return ProviderOperationObservation{}, fmt.Errorf("provider observation %s does not match the durable operation", field.name)
		}
	}
	observation.AttemptID = operation.AttemptID
	observation.ProviderConnectionID = operation.ProviderConnectionID
	observation.ProviderConnectionRevision = operation.ProviderConnectionRevision
	observation.ProviderNamespaceID = operation.ProviderNamespaceID
	observation.Provider = operation.Provider
	if observation.Source != ObservationSourceSale && observation.Source != ObservationSourceQuery && observation.Source != ObservationSourceWebhook {
		return ProviderOperationObservation{}, errors.New("provider observation has an unsupported runtime source")
	}
	if strings.TrimSpace(observation.RawProviderStatus) == "" || strings.ContainsRune(observation.RawProviderStatus, '\x00') {
		return ProviderOperationObservation{}, errors.New("provider observation raw status is required")
	}
	if !validObservationStatus(observation.NormalizedStatus) || !validObservationCertainty(observation.NormalizedCertainty) {
		return ProviderOperationObservation{}, errors.New("provider observation normalization is invalid")
	}
	if observation.NormalizedStatus == StatusCreated || observation.NormalizedStatus == StatusSending {
		return ProviderOperationObservation{}, errors.New("runtime provider observation cannot create dispatch state")
	}
	if observation.Source == ObservationSourceWebhook && observation.ProviderEventID == "" {
		return ProviderOperationObservation{}, errors.New("webhook observation provider event id is required")
	}
	for _, value := range []string{
		observation.NormalizedProviderReference, observation.NormalizedFailureCode, observation.ProviderEventID,
	} {
		if strings.ContainsRune(value, '\x00') {
			return ProviderOperationObservation{}, errors.New("provider observation text fields cannot contain NUL bytes")
		}
	}
	if observation.ObservedAt.IsZero() {
		return ProviderOperationObservation{}, errors.New("provider observation observed_at is required")
	}
	var object map[string]any
	if err := json.Unmarshal(observation.SanitizedPayload, &object); err != nil || object == nil {
		return ProviderOperationObservation{}, errors.New("provider observation sanitized payload must be a JSON object")
	}
	if containsSensitiveObservationField(object) {
		return ProviderOperationObservation{}, errors.New("provider observation sanitized payload contains a sensitive field")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return ProviderOperationObservation{}, err
	}
	// The boundary accepts at most 64 KiB. JSON string escaping can expand a
	// sanitized representation, so keep enough room for the worst-case six-byte
	// escape plus a small structural margin.
	if len(canonical) > (maxProviderResponseBytes*6)+1024 {
		return ProviderOperationObservation{}, errors.New("provider observation sanitized payload is too large")
	}
	observation.SanitizedPayload = canonical
	hash := sha256.Sum256(canonical)
	observation.PayloadHash = hex.EncodeToString(hash[:])
	observation.ObservedAt = observation.ObservedAt.UTC()
	return observation, nil
}

func containsSensitiveObservationField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
			switch normalized {
			case "authorization", "credential", "credentials", "cvv", "cvc", "pan", "password", "payment_method_token", "secret", "token":
				return true
			}
			if containsSensitiveObservationField(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsSensitiveObservationField(nested) {
				return true
			}
		}
	}
	return false
}

func validObservationStatus(status string) bool {
	switch status {
	case StatusCreated, StatusSending, StatusUnknown, StatusSucceeded, StatusFailed, StatusReview:
		return true
	default:
		return false
	}
}

func validObservationCertainty(certainty string) bool {
	return certainty == "none" || certainty == "possible" || certainty == "confirmed"
}

func providerResponseObservation(
	operationID string,
	source string,
	response providerResponse,
	status string,
	certainty string,
	reference string,
	failureCode string,
) ProviderOperationObservation {
	rawStatus := response.Status
	if rawStatus == "" {
		rawStatus = "missing_status"
	}
	return ProviderOperationObservation{
		OperationID: operationID, Source: source, RawProviderStatus: rawStatus,
		NormalizedStatus: status, NormalizedCertainty: certainty,
		NormalizedProviderReference: reference, NormalizedFailureCode: failureCode,
		SanitizedPayload: mustJSON(response), ObservedAt: time.Now().UTC(),
	}
}

type providerCallDiagnosticError struct {
	category   string
	statusCode int
	bodyHash   string
	byteCount  int
	truncated  bool
	cause      error
}

func (e *providerCallDiagnosticError) Error() string {
	return fmt.Sprintf("provider response %s (http_status=%d bytes=%d): %v", e.category, e.statusCode, e.byteCount, e.cause)
}

func (e *providerCallDiagnosticError) Unwrap() error { return e.cause }

func providerCallErrorObservation(operationID, source, failureCode string, callErr error) ProviderOperationObservation {
	payload := map[string]any{"kind": "provider_call_error", "category": "transport_error"}
	rawStatus := "transport_error"
	var diagnostic *providerCallDiagnosticError
	if errors.As(callErr, &diagnostic) {
		payload["category"] = diagnostic.category
		payload["status_code"] = diagnostic.statusCode
		if diagnostic.truncated {
			payload["observed_prefix_sha256"] = diagnostic.bodyHash
			payload["observed_prefix_byte_count"] = diagnostic.byteCount
			payload["truncated"] = true
		} else {
			payload["body_sha256"] = diagnostic.bodyHash
			payload["byte_count"] = diagnostic.byteCount
		}
		rawStatus = diagnostic.category
	}
	return ProviderOperationObservation{
		OperationID: operationID, Source: source,
		RawProviderStatus: rawStatus, NormalizedStatus: StatusUnknown,
		NormalizedCertainty: "possible", NormalizedFailureCode: failureCode,
		SanitizedPayload: mustJSON(payload),
		ObservedAt:       time.Now().UTC(),
	}
}

func providerAbsenceObservation(operationID string) ProviderOperationObservation {
	return ProviderOperationObservation{
		OperationID: operationID, Source: ObservationSourceQuery,
		RawProviderStatus: "operation_not_found", NormalizedStatus: StatusUnknown,
		NormalizedCertainty: "none", NormalizedFailureCode: "provider_operation_not_found",
		SanitizedPayload: mustJSON(map[string]any{"error": map[string]string{"code": "operation_not_found"}}),
		ObservedAt:       time.Now().UTC(),
	}
}

func webhookProviderObservation(event VerifiedWebhookEvent, fallback ProviderOutcome) ProviderOperationObservation {
	rawStatus := event.RawProviderStatus
	if rawStatus == "" {
		rawStatus = event.Status
	}
	payload := event.SanitizedPayload
	if len(payload) == 0 {
		payload = mustJSON(struct {
			RawProviderStatus string          `json:"raw_provider_status"`
			Outcome           ProviderOutcome `json:"normalized_outcome"`
		}{RawProviderStatus: rawStatus, Outcome: fallback})
	}
	observedAt := event.OccurredAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	return ProviderOperationObservation{
		OperationID: event.OperationID, Source: ObservationSourceWebhook,
		RawProviderStatus: rawStatus, NormalizedStatus: event.Status,
		NormalizedCertainty:         event.SideEffectCertainty,
		NormalizedProviderReference: event.ProviderReference,
		NormalizedFailureCode:       event.FailureCode, ProviderEventID: event.EventID,
		SanitizedPayload: payload, ObservedAt: observedAt,
	}
}
