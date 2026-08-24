package postgres

import (
	"errors"
	"testing"

	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/jackc/pgx/v5"
)

type fakeRow struct {
	values []any
	err    error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	for i, value := range f.values {
		switch target := dest[i].(type) {
		case *string:
			*target = value.(string)
		case *[]byte:
			*target = value.([]byte)
		case *int:
			*target = value.(int)
		default:
			panic("unsupported fake scan target")
		}
	}
	return nil
}

func TestScanIdempotencyReturnsStableCompletedResponse(t *testing.T) {
	hash := idempotency.Fingerprint("create_payment_intent", []byte(`{"amount_minor":100}`))
	lookup, err := scanIdempotency(fakeRow{values: []any{
		"create_payment_intent", hash, "completed", "pi_1", 201, []byte(`{"id":"pi_1"}`),
	}}, "create_payment_intent", hash)
	if err != nil {
		t.Fatal(err)
	}
	if !lookup.Found || lookup.InProgress || lookup.Response.Status != 201 || string(lookup.Response.Body) != `{"id":"pi_1"}` {
		t.Fatalf("unexpected replay: %+v", lookup)
	}
}

func TestScanIdempotencyRejectsReusedKey(t *testing.T) {
	storedHash := idempotency.Fingerprint("create_payment_intent", []byte(`{"amount_minor":100}`))
	requestedHash := idempotency.Fingerprint("create_payment_intent", []byte(`{"amount_minor":200}`))
	_, err := scanIdempotency(fakeRow{values: []any{
		"create_payment_intent", storedHash, "completed", "pi_1", 201, []byte(`{}`),
	}}, "create_payment_intent", requestedHash)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v, want ErrIdempotencyConflict", err)
	}
}

func TestScanIdempotencyMissing(t *testing.T) {
	lookup, err := scanIdempotency(fakeRow{err: pgx.ErrNoRows}, "confirm_payment_intent:pi_1", []byte("hash"))
	if err != nil || lookup.Found {
		t.Fatalf("unexpected missing lookup: %+v err=%v", lookup, err)
	}
}
