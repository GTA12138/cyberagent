package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"
)

type migration struct {
	version   int
	name, sql string
}

// Applied migrations are immutable. Add a new version instead of editing an
// existing SQL body. CRLF/LF is normalized so Windows/Linux share one checksum.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrations() []migration {
	names := []string{"0001_legacy_baseline.sql", "0002_model_records.sql", "0003_execution_attempts.sql", "0004_trigger_deliveries.sql", "0005_trigger_outbox.sql", "0006_chengjian_identity.sql"}
	out := make([]migration, 0, len(names))
	for i, name := range names {
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			panic(err)
		}
		out = append(out, migration{i + 1, name, string(body)})
	}
	return out
}
func normalizeSQL(body string) string { return strings.ReplaceAll(body, "\r\n", "\n") }

func migrate(ctx context.Context, conn *sql.Conn, versions []migration) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `SELECT version,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	applied := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(applied) > len(versions) {
		return fmt.Errorf("database schema is newer than this binary")
	}
	for v := range applied {
		if v < 1 || v > len(versions) {
			return fmt.Errorf("unsupported applied migration %d", v)
		}
	}
	for v := 1; v <= len(applied); v++ {
		if _, ok := applied[v]; !ok {
			return fmt.Errorf("missing applied migration %d", v)
		}
	}
	for index, m := range versions {
		if m.version != index+1 {
			return fmt.Errorf("migration versions must be contiguous: %d", m.version)
		}
		sum := migrationChecksum(m.sql)
		if old, ok := applied[m.version]; ok {
			if old != sum {
				return fmt.Errorf("migration %d (%s) checksum mismatch; add a new migration instead of editing applied SQL", m.version, m.name)
			}
			continue
		}
		for attempt := 0; ; attempt++ {
			err = applyMigration(ctx, conn, m, sum)
			if err == nil {
				break
			}
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
			}
			timer := time.NewTimer(schemaDeadlockRetryDelays[attempt])
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	for v := range applied {
		if v < 1 || v > len(versions) {
			return fmt.Errorf("unsupported applied migration %d", v)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, m migration, sum string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES ($1,$2,$3)`, m.version, m.name, sum); err != nil {
		return err
	}
	return tx.Commit()
}

func migrationChecksum(body string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(normalizeSQL(body))))
}
