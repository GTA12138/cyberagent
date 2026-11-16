package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func sqliteIndexDSN(index string, readOnly bool) (string, error) {
	abs, err := filepath.Abs(index)
	if err != nil {
		return "", err
	}
	path := filepath.ToSlash(abs)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// OpenReader opens an existing capture without starting a proxy, generating a CA,
// or upgrading its schema. In particular, looking up history must not create a
// new task capture when a task has already finished.
func OpenReader(ctx context.Context, dir string) (*Traffic, error) {
	index, err := filepath.Abs(filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(index); err != nil {
		return nil, fmt.Errorf("流量历史不可用: %w", err)
	}
	dsn, err := sqliteIndexDSN(index, true)
	if err != nil {
		return nil, err
	}
	indexDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Traffic, error) { _ = indexDB.Close(); return nil, err }
	if err = indexDB.PingContext(ctx); err != nil {
		return fail(err)
	}
	var count int
	if err = indexDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('exchanges','exchange_bodies')`).Scan(&count); err != nil {
		return fail(err)
	}
	if count != 2 {
		return fail(fmt.Errorf("流量历史索引缺少必需表；请先由录制器完成升级"))
	}
	var fts int
	if err = indexDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ex_fts'`).Scan(&fts); err != nil {
		return fail(err)
	}
	return &Traffic{dir: dir, db: indexDB, fts: fts != 0, closed: make(chan struct{})}, nil
}
