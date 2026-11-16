package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"

	"chengjian.local/chengjian/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var ErrInstanceRunning = errors.New("another CHENGJIAN instance already owns this database")

// 1001 migration, 1002 test suite, 1003 company scope, 1004 evidence.
const instanceLockKey int64 = 7337741005

// OpenOptions controls bounded startup and database resource usage. Exclusive is
// required by the server; ordinary Open remains available to offline tools/tests.
type OpenOptions struct {
	Exclusive        bool
	MaxOpenConns     int
	MaxIdleConns     int
	ConnectTimeout   time.Duration
	QueryTimeout     time.Duration
	MigrationTimeout time.Duration
}

func RuntimeOpenOptions() OpenOptions {
	c := config.Load().Database
	return OpenOptions{Exclusive: true, MaxOpenConns: c.MaxOpenConns, MaxIdleConns: c.MaxIdleConns,
		ConnectTimeout:   time.Duration(c.ConnectTimeoutSeconds) * time.Second,
		QueryTimeout:     time.Duration(c.QueryTimeoutSeconds) * time.Second,
		MigrationTimeout: time.Duration(c.MigrationTimeoutSeconds) * time.Second}
}

func (o OpenOptions) normalized() (OpenOptions, error) {
	if o.MaxOpenConns == 0 {
		o.MaxOpenConns = 16
	}
	if o.MaxIdleConns == 0 {
		o.MaxIdleConns = 4
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 5 * time.Second
	}
	if o.QueryTimeout == 0 {
		o.QueryTimeout = 30 * time.Second
	}
	if o.MigrationTimeout == 0 {
		o.MigrationTimeout = 2 * time.Minute
	}
	if o.MaxOpenConns < 1 || o.MaxIdleConns < 0 || o.ConnectTimeout < 0 || o.QueryTimeout < 0 || o.MigrationTimeout < 0 {
		return o, errors.New("database pool sizes and timeouts must be positive")
	}
	if o.Exclusive && o.MaxOpenConns < 2 {
		return o, errors.New("single-instance ownership reserves one connection; max_open_conns must be at least 2")
	}
	if o.MaxIdleConns > o.MaxOpenConns {
		o.MaxIdleConns = o.MaxOpenConns
	}
	return o, nil
}

// OpenContext obtains ownership BEFORE migrations/seeding/recovery. Seeding uses
// the migration connection itself, so a small pool cannot deadlock at startup.
func OpenContext(parent context.Context, dsn string, options OpenOptions) (_ *DB, err error) {
	o, err := options.normalized()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, o.ConnectTimeout)
	err = ensureDatabase(ctx, dsn)
	cancel()
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	cfg.ConnectTimeout = o.ConnectTimeout
	cfg.RuntimeParams["statement_timeout"] = fmt.Sprint(o.QueryTimeout.Milliseconds())
	cfg.RuntimeParams["lock_timeout"] = fmt.Sprint(o.QueryTimeout.Milliseconds())
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = fmt.Sprint((time.Minute).Milliseconds())
	sqlDB := stdlib.OpenDB(*cfg)
	sqlDB.SetMaxOpenConns(o.MaxOpenConns)
	sqlDB.SetMaxIdleConns(o.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	d := &DB{DB: sqlDB}
	defer func() {
		if err != nil {
			_ = d.Close()
		}
	}()
	ctx, cancel = context.WithTimeout(parent, o.ConnectTimeout)
	conn, err := sqlDB.Conn(ctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = conn.Close()
		}
	}()
	ctx, cancel = context.WithTimeout(parent, o.ConnectTimeout)
	if o.Exclusive {
		var owned bool
		err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, instanceLockKey).Scan(&owned)
		if err == nil && !owned {
			err = ErrInstanceRunning
		}
	}
	cancel()
	if err != nil {
		return nil, err
	}
	if o.Exclusive {
		d.instance = &instanceLease{conn: conn, stop: make(chan struct{}), done: make(chan struct{}), lost: make(chan struct{})}
		keep = true
	}
	ctx, cancel = context.WithTimeout(parent, o.MigrationTimeout)
	err = initializeConnection(ctx, conn, o)
	cancel()
	if err != nil {
		return nil, err
	}
	if d.instance != nil {
		d.instance.started = true
		go d.instance.watch()
	}
	return d, nil
}

func initializeConnection(ctx context.Context, conn *sql.Conn, o OpenOptions) (err error) {
	// DDL gets the bounded migration budget, then the runtime timeout is restored.
	if _, err := conn.ExecContext(ctx, `SELECT set_config('statement_timeout',$1,false)`, fmt.Sprint(o.MigrationTimeout.Milliseconds())); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, unlockErr := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil {
			// A connection with an uncertain session lock must never return to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("release migration lock: %w", unlockErr)
			}
		}
	}()
	if err := migrate(ctx, conn, migrations()); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := (&builtinSeeder{ctx: ctx, tx: tx}).seedBuiltins(); err != nil {
		return fmt.Errorf("seed builtins: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `SELECT set_config('statement_timeout',$1,false)`, fmt.Sprint(o.QueryTimeout.Milliseconds()))
	return err
}

type instanceLease struct {
	conn             *sql.Conn
	stop, done, lost chan struct{}
	once             sync.Once
	started          bool
	lostOnce         sync.Once
}

func (l *instanceLease) watch() {
	defer close(l.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := l.conn.ExecContext(ctx, `SELECT 1`)
			cancel()
			if err != nil {
				l.lostOnce.Do(func() { close(l.lost) })
				return
			}
		}
	}
}

// OwnershipLost is nil for offline handles. The server cancels all runners when
// the dedicated ownership connection fails or is released; restart to reacquire.
func (d *DB) OwnershipLost() <-chan struct{} {
	if d == nil || d.instance == nil {
		return nil
	}
	return d.instance.lost
}

func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		if d.instance != nil {
			l := d.instance
			l.lostOnce.Do(func() { close(l.lost) })
			l.once.Do(func() { close(l.stop) })
			if l.started {
				<-l.done
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, instanceLockKey)
			cancel()
			_ = l.conn.Close()
		}
		if d.DB != nil {
			d.closeErr = d.DB.Close()
		}
	})
	return d.closeErr
}

type builtinSeeder struct {
	ctx context.Context
	tx  *sql.Tx
}

func (s *builtinSeeder) Exec(q string, args ...any) (sql.Result, error) {
	return s.tx.ExecContext(s.ctx, q, args...)
}
func (s *builtinSeeder) QueryRow(q string, args ...any) *sql.Row {
	return s.tx.QueryRowContext(s.ctx, q, args...)
}
func (s *builtinSeeder) GetSetting(key string) (value string, ok bool, err error) {
	err = s.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}
func (s *builtinSeeder) SetSetting(key, value string) error {
	_, err := s.Exec(`INSERT INTO settings(key,value) VALUES ($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, key, value)
	return err
}
