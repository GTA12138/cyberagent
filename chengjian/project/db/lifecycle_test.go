package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func lifecycleTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestVersionedMigrationRollbackAndIntegrity(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	schema := fmt.Sprintf("migration_test_%d", time.Now().UnixNano())
	if _, err = conn.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SET search_path TO public`)
		_, _ = conn.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	}()
	if _, err = conn.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	versions := []migration{{1, "first", "CREATE TABLE example(id INT PRIMARY KEY); INSERT INTO example VALUES(1)"}}
	if err = migrate(ctx, conn, versions); err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, conn, versions); err != nil {
		t.Fatalf("second startup must skip applied SQL: %v", err)
	}
	edited := []migration{{1, "first", versions[0].sql + "; SELECT 1"}}
	if err = migrate(ctx, conn, edited); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("edited migration accepted: %v", err)
	}
	broken := append(versions, migration{2, "broken", "CREATE TABLE rolled_back(id INT); SELECT 1/0"})
	if err = migrate(ctx, conn, broken); err == nil {
		t.Fatal("broken migration accepted")
	}
	var table sql.NullString
	var count int
	if err = conn.QueryRowContext(ctx, `SELECT to_regclass('rolled_back')::text`).Scan(&table); err != nil || table.Valid {
		t.Fatalf("failed migration leaked DDL: %v %v", table, err)
	}
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed migration recorded: %d %v", count, err)
	}
	if migrationChecksum("SELECT 1\r\n") != migrationChecksum("SELECT 1\n") {
		t.Fatal("platform line endings change checksum")
	}
}

func TestExclusiveInstanceAndSmallPool(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// A single initialization connection must seed without requesting a second slot.
	one, err := OpenContext(ctx, dsn, OpenOptions{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = one.Close()
	opts := OpenOptions{Exclusive: true, MaxOpenConns: 2}
	first, err := OpenContext(ctx, dsn, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Ownership must not block unrelated transactions on another pool connection.
	business, err := first.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []int64{schemaMigrationLockKey, companyScopeMutationLock, findingEvidenceLockKey, 7337741010, 7337741011} {
		var acquired bool
		if err = business.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil || !acquired {
			business.Close()
			t.Fatalf("ownership blocks business lock %d: %v", key, err)
		}
		if _, err = business.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			business.Close()
			t.Fatal(err)
		}
	}
	business.Close()
	second, err := OpenContext(ctx, dsn, opts)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, ErrInstanceRunning) {
		t.Fatalf("second runtime accepted: %v", err)
	}
	var pid int
	if err = first.instance.conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = first.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.OwnershipLost():
	case <-time.After(6 * time.Second):
		t.Fatal("lost ownership not reported")
	}
	_ = first.Close()
	replacement, err := OpenContext(ctx, dsn, opts)
	if err != nil {
		t.Fatal(err)
	}
	released := replacement.OwnershipLost()
	_ = replacement.Close()
	select {
	case <-released:
	default:
		t.Fatal("voluntary close left ownership observers running")
	}
}

func TestMigrationsAdoptUnversionedDatabase(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	schema := fmt.Sprintf("legacy_test_%d", time.Now().UnixNano())
	if _, err = conn.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SET search_path TO public`)
		_, _ = conn.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	}()
	if _, err = conn.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES ('legacy_marker','preserve')`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, conn, migrations()); err != nil {
		t.Fatal(err)
	}
	var value string
	var count int
	if err = conn.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='legacy_marker'`).Scan(&value); err != nil || value != "preserve" {
		t.Fatalf("legacy data changed: %q %v", value, err)
	}
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != len(migrations()) {
		t.Fatalf("adoption not versioned: %d %v", count, err)
	}
}
