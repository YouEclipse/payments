package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockID int64 = 710_130_612

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer connection.Release()

	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()

	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS payment_schema_migrations (
			version text PRIMARY KEY,
			checksum bytea,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
		ALTER TABLE payment_schema_migrations
			ADD COLUMN IF NOT EXISTS checksum bytea`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("list embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		checksum := sha256.Sum256(body)
		var storedChecksum []byte
		err = connection.QueryRow(ctx,
			"SELECT checksum FROM payment_schema_migrations WHERE version=$1",
			entry.Name(),
		).Scan(&storedChecksum)
		if err == nil {
			if len(storedChecksum) == 0 {
				if _, err := connection.Exec(ctx,
					"UPDATE payment_schema_migrations SET checksum=$2 WHERE version=$1 AND checksum IS NULL",
					entry.Name(), checksum[:],
				); err != nil {
					return fmt.Errorf("backfill migration checksum %s: %w", entry.Name(), err)
				}
				continue
			}
			if !bytes.Equal(storedChecksum, checksum[:]) {
				return fmt.Errorf("migration %s checksum does not match the applied version", entry.Name())
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", entry.Name(), err)
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO payment_schema_migrations(version,checksum) VALUES($1,$2)", entry.Name(), checksum[:],
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
