package db

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRebrandSecretCompatibility(t *testing.T) {
	master := []byte("compatibility-fixture-master-key")
	for _, kind := range []string{"sessions", "credentials"} {
		t.Run(kind, func(t *testing.T) {
			// Independently construct the pre-rebrand storage format.
			sum := sha256.Sum256(append([]byte("artex/"+kind+"/v1\x00"), master...))
			gcm, err := newGCM(sum[:])
			if err != nil {
				t.Fatal(err)
			}
			nonce := make([]byte, gcm.NonceSize())
			legacy := "gcm1:" + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte("fixture-secret"), nil))
			session := NewSessionStore(nil, master)
			credential := NewCredentialStore(nil, master)
			var got, current string
			if kind == "sessions" {
				plain, e := session.decrypt(legacy)
				err = e
				got = string(plain)
				current, _ = session.encrypt([]byte("fixture-secret"))
				if _, e := NewSessionStore(nil, []byte("wrong-key")).decrypt(legacy); e == nil {
					t.Fatal("wrong key accepted")
				}
				if _, e := credential.decrypt(legacy); e == nil {
					t.Fatal("cross-domain key accepted")
				}
			} else {
				got, err = credential.decrypt(legacy)
				current, _ = credential.encrypt("fixture-secret")
				if _, e := NewCredentialStore(nil, []byte("wrong-key")).decrypt(legacy); e == nil {
					t.Fatal("wrong key accepted")
				}
				if _, e := session.decrypt(legacy); e == nil {
					t.Fatal("cross-domain key accepted")
				}
			}
			if err != nil || got != "fixture-secret" {
				t.Fatalf("legacy decryption failed: %v", err)
			}
			if !strings.HasPrefix(current, "gcm2:") {
				t.Fatal("new writes must identify the new KDF")
			}
			// Relabeling authenticated ciphertext must never fall back to another key.
			relabeled := "gcm2:" + strings.TrimPrefix(legacy, "gcm1:")
			if kind == "sessions" {
				_, err = session.decrypt(relabeled)
			} else {
				_, err = credential.decrypt(relabeled)
			}
			if err == nil {
				t.Fatal("relabeled ciphertext accepted")
			}
		})
	}
}

func TestRebrandMigratesExistingDatabase(t *testing.T) {
	d := lifecycleTestDB(t)
	ctx := t.Context()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	schema := fmt.Sprintf("rebrand_test_%d", time.Now().UnixNano())
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
	versions := migrations()
	if err = migrate(ctx, conn, versions[:5]); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES ('before_rebrand','preserved')`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(ctx, conn, versions); err != nil {
		t.Fatalf("existing database upgrade: %v", err)
	}
	if err = migrate(ctx, conn, versions); err != nil {
		t.Fatalf("repeated upgrade: %v", err)
	}
	var value string
	if err = conn.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='before_rebrand'`).Scan(&value); err != nil || value != "preserved" {
		t.Fatal("existing data changed", err)
	}
	var count int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'chengjian_%' AND NOT tgisinternal AND tgrelid IN ('tasks'::regclass,'exploration_nodes'::regclass,'activity'::regclass)`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("unexpected trigger count %d: %v", count, err)
	}
	// Exercise the stored PL/pgSQL bodies too; renaming SQL objects alone leaves
	// references inside function bodies pointing to the old names.
	if _, err = conn.ExecContext(ctx, `WITH e AS (INSERT INTO explorations(goal) VALUES ('test') RETURNING id)
INSERT INTO tasks(description,goal,exploration_id) SELECT 'upgrade fixture','test',id FROM e`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `SELECT chengjian_emit_trigger_event('interval','{}'::jsonb,NULL)`); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM trigger_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("event duplicated or missing %d: %v", count, err)
	}
}
