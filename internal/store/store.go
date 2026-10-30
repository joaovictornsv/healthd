// Package store persists samples and alert events in SQLite.
//
// Retention: raw samples 24h, 1-minute averages 30 days, alert events 90 days.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/joaovictornsv/healthd/internal/api"
)

const (
	rawRetention    = 24 * time.Hour
	minuteRetention = 30 * 24 * time.Hour
	eventRetention  = 90 * 24 * time.Hour
)

const schema = `
CREATE TABLE IF NOT EXISTS samples (
	ts INTEGER NOT NULL,
	indicator TEXT NOT NULL,
	value REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS samples_indicator_ts ON samples (indicator, ts);

CREATE TABLE IF NOT EXISTS samples_1m (
	ts INTEGER NOT NULL,
	indicator TEXT NOT NULL,
	value REAL NOT NULL,
	PRIMARY KEY (indicator, ts)
);

CREATE TABLE IF NOT EXISTS events (
	ts INTEGER NOT NULL,
	indicator TEXT NOT NULL,
	kind TEXT NOT NULL,
	critical INTEGER NOT NULL,
	value REAL NOT NULL,
	message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_ts ON events (ts);

CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
}

type Sample struct {
	Indicator string
	Value     float64
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AddSamples(t time.Time, samples []Sample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO samples (ts, indicator, value) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, sm := range samples {
		if _, err := stmt.Exec(t.Unix(), sm.Indicator, sm.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddEvent(e api.Event) error {
	_, err := s.db.Exec(`INSERT INTO events (ts, indicator, kind, critical, value, message) VALUES (?, ?, ?, ?, ?, ?)`,
		e.Time.Unix(), e.Indicator, e.Kind, e.Critical, e.Value, e.Message)
	return err
}

// Events returns the newest events first. Empty indicator means all.
func (s *Store) Events(indicator string, limit int) ([]api.Event, error) {
	rows, err := s.db.Query(`
		SELECT ts, indicator, kind, critical, value, message FROM events
		WHERE ? = '' OR indicator = ?
		ORDER BY ts DESC, rowid DESC LIMIT ?`, indicator, indicator, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Event
	for rows.Next() {
		var e api.Event
		var ts int64
		if err := rows.Scan(&ts, &e.Indicator, &e.Kind, &e.Critical, &e.Value, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Meta returns a stored value, or "" if the key is not set.
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, key, value)
	return err
}

// Raw returns raw samples newer than since, oldest first.
func (s *Store) Raw(indicator string, since time.Time) ([]api.Point, error) {
	return s.points(`SELECT ts, value FROM samples WHERE indicator = ? AND ts >= ? ORDER BY ts`,
		indicator, since.Unix())
}

// History returns the last span of samples averaged over buckets of the
// given size, oldest first. Spans within the raw retention read raw samples;
// longer spans read the 1-minute averages.
func (s *Store) History(indicator string, span, bucket time.Duration) ([]api.Point, error) {
	since := time.Now().Add(-span)
	table := "samples"
	if span > rawRetention {
		table = "samples_1m"
	}
	b := int64(bucket.Seconds())
	if b < 1 {
		b = 1
	}
	return s.points(fmt.Sprintf(`
		SELECT (ts / %[1]d) * %[1]d AS bucket, AVG(value) FROM %[2]s
		WHERE indicator = ? AND ts >= ?
		GROUP BY bucket ORDER BY bucket`, b, table),
		indicator, since.Unix())
}

func (s *Store) points(query string, args ...any) ([]api.Point, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Point
	for rows.Next() {
		var p api.Point
		if err := rows.Scan(&p.T, &p.V); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Maintain rolls raw samples up into 1-minute averages and applies retention.
func (s *Store) Maintain(now time.Time) error {
	var last int64
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'rolled_up_to'`).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		last = now.Add(-rawRetention).Unix()
	case err != nil:
		return err
	default:
		last, _ = strconv.ParseInt(v, 10, 64)
	}
	// Only roll up whole minutes that have ended.
	upTo := now.Truncate(time.Minute).Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT OR REPLACE INTO samples_1m (ts, indicator, value)
		  SELECT (ts / 60) * 60, indicator, AVG(value) FROM samples
		  WHERE ts >= ? AND ts < ? GROUP BY 1, 2`, []any{last, upTo}},
		{`INSERT OR REPLACE INTO meta (key, value) VALUES ('rolled_up_to', ?)`, []any{strconv.FormatInt(upTo, 10)}},
		{`DELETE FROM samples WHERE ts < ?`, []any{now.Add(-rawRetention).Unix()}},
		{`DELETE FROM samples_1m WHERE ts < ?`, []any{now.Add(-minuteRetention).Unix()}},
		{`DELETE FROM events WHERE ts < ?`, []any{now.Add(-eventRetention).Unix()}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(st.q, st.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}
