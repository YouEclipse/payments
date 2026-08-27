package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const transactionAttempts = 8

// Each invocation must open a fresh transaction, reuse the already allocated
// command IDs, and contain no network/Router/Provider calls. Only an aborted
// serializable/deadlock transaction is replayable; connection-loss on COMMIT
// has an ambiguous result and must not be retried here.
func retryTransaction[T any](ctx context.Context, execute func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < transactionAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		result, err := execute()
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || (pgError.Code != "40001" && pgError.Code != "40P01") || attempt == transactionAttempts-1 {
			return result, err
		}
		timer := time.NewTimer(time.Millisecond * time.Duration(1<<min(attempt, 5)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable transaction retry")
}
