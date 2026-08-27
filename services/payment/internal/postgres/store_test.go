package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/idempotency"
	"github.com/jackc/pgx/v5"
)

func TestDatabaseClockMustStayInsideTheMeasuredProcessWindow(t *testing.T) {
	start := time.Date(2026, 8, 27, 1, 2, 3, 0, time.UTC)
	finish := start.Add(100 * time.Millisecond)
	budget := 500 * time.Millisecond
	for _, test := range []struct {
		name       string
		databaseAt time.Time
		accepted   bool
	}{
		{name: "inside query interval", databaseAt: start.Add(50 * time.Millisecond), accepted: true},
		{name: "at fast-clock budget edge", databaseAt: finish.Add(-budget), accepted: true},
		{name: "at slow-clock budget edge", databaseAt: start.Add(budget), accepted: true},
		{name: "process clock too fast", databaseAt: finish.Add(-budget - time.Nanosecond)},
		{name: "process clock too slow", databaseAt: start.Add(budget + time.Nanosecond)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := databaseClockWithinProcessWindow(test.databaseAt, start, finish, budget); got != test.accepted {
				t.Fatalf("accepted=%v, want %v", got, test.accepted)
			}
		})
	}
	if databaseClockWithinProcessWindow(start, finish, start, budget) {
		t.Fatal("accepted a reversed process-clock window")
	}
	if databaseClockWithinProcessWindow(start, start, finish, 0) {
		t.Fatal("accepted a zero clock-skew budget")
	}
	// An asymmetric response delay cannot hide a process clock that is two
	// seconds slow: every possible offset must be safe, not merely one.
	if databaseClockWithinProcessWindow(start.Add(2*time.Second), start, start.Add(3*time.Second), budget) {
		t.Fatal("network uncertainty masked an unsafe slow process clock")
	}
	if databaseClockWithinProcessWindow(start.Add(550*time.Millisecond), start, start.Add(1100*time.Millisecond), budget) {
		t.Fatal("round-trip uncertainty larger than twice the budget was accepted")
	}
}

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
		"create_payment_intent", hash, "completed", "pi_1", 201, []byte(`{"id":"pi_1"}`), idempotency.FingerprintVersion,
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
		"create_payment_intent", storedHash, "completed", "pi_1", 201, []byte(`{}`), idempotency.FingerprintVersion,
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

func TestProviderReferenceLockIDUsesAnUnambiguousPairEncoding(t *testing.T) {
	first := providerReferenceLockID("provider-a", "reference-b")
	if first != providerReferenceLockID("provider-a", "reference-b") {
		t.Fatal("the same provider/reference pair must produce a stable lock ID")
	}
	if first == providerReferenceLockID("provider", "-areference-b") {
		t.Fatal("different provider/reference pairs must not collapse through string concatenation")
	}
}
