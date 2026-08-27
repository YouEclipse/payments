package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTransactionRetryUsesFreshAttemptOnlyForKnownAbortCodes(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		attempts := 0
		got, err := retryTransaction(t.Context(), func() (string, error) {
			attempts++
			if attempts < 3 {
				return "", &pgconn.PgError{Code: code}
			}
			return "committed", nil
		})
		if err != nil || got != "committed" || attempts != 3 {
			t.Fatalf("code=%s result=%q attempts=%d err=%v", code, got, attempts, err)
		}
	}
	attempts := 0
	_, err := retryTransaction(t.Context(), func() (string, error) {
		attempts++
		return "", &pgconn.PgError{Code: "08006"}
	})
	if err == nil || attempts != 1 {
		t.Fatalf("ambiguous connection error was retried: attempts=%d err=%v", attempts, err)
	}
}

func TestTransactionRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := retryTransaction(ctx, func() (struct{}, error) {
		attempts++
		cancel()
		return struct{}{}, &pgconn.PgError{Code: "40001"}
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("canceled retry attempts=%d err=%v", attempts, err)
	}
}
