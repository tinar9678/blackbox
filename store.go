package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// timeFormat is fixed-width so timestamps sort correctly as strings.
const timeFormat = "2006-01-02T15:04:05.000000Z"

// Call is one recorded request/response pair.
type Call struct {
	ID           int64    `json:"id"`
	StartedAt    string   `json:"started_at"`
	Provider     string   `json:"provider"`
	Method       string   `json:"method"`
	Path         string   `json:"path"`
	Model        string   `json:"model"`
	Tag          string   `json:"tag"`
	Status       int      `json:"status"`
	Streamed     bool     `json:"streamed"`
	DurationMS   int64    `json:"duration_ms"`
	TTFTMS       *int64   `json:"ttft_ms"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CacheRead    int64    `json:"cache_read_tokens"`
	CacheWrite   int64    `json:"cache_write_tokens"`
	CostUSD      *float64 `json:"cost_usd"`
	Error        string   `json:"error,omitempty"`

	// Only populated for single-call fetches.
	RequestHeaders  string `json:"request_headers,omitempty"`
	RequestBody     string `json:"request_body,omitempty"`
	ResponseHeaders string `json:"response_headers,omitempty"`
	ResponseBody    string `json:"response_body,omitempty"`
	ResponseText    string `json:"response_text,omitempty"`
}

type Store struct{ db *sql.DB }

// migrations[i] upgrades the schema from version i to i+1. Append only:
// never edit a migration that has shipped, or existing databases break.
var migrations = []string{
	`CREATE TABLE calls (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		started_at       TEXT    NOT NULL,
		provider         TEXT    NOT NULL,
		method           TEXT    NOT NULL,
		path             TEXT    NOT NULL,
		model            TEXT    NOT NULL DEFAULT '',
		tag              TEXT    NOT NULL DEFAULT '',
		status           INTEGER NOT NULL DEFAULT 0,
		streamed         INTEGER NOT NULL DEFAULT 0,
		duration_ms      INTEGER NOT NULL DEFAULT 0,
		ttft_ms          INTEGER,
		input_tokens     INTEGER NOT NULL DEFAULT 0,
		output_tokens    INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		cost_usd         REAL,
		error            TEXT    NOT NULL DEFAULT '',
		request_headers  TEXT    NOT NULL DEFAULT '',
		request_body     TEXT    NOT NULL DEFAULT '',
		response_headers TEXT    NOT NULL DEFAULT '',
		response_body    TEXT    NOT NULL DEFAULT '',
		response_text    TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX calls_started_at ON calls(started_at);
	CREATE INDEX calls_tag ON calls(tag);`,
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialize to avoid SQLITE_BUSY.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this blackbox (v%d); upgrade blackbox", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Insert(c *Call) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO calls (
		started_at, provider, method, path, model, tag, status, streamed, duration_ms, ttft_ms,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, error,
		request_headers, request_body, response_headers, response_body, response_text
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.StartedAt, c.Provider, c.Method, c.Path, c.Model, c.Tag, c.Status, c.Streamed, c.DurationMS, c.TTFTMS,
		c.InputTokens, c.OutputTokens, c.CacheRead, c.CacheWrite, c.CostUSD, c.Error,
		c.RequestHeaders, c.RequestBody, c.ResponseHeaders, c.ResponseBody, c.ResponseText)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const summaryCols = `id, started_at, provider, method, path, model, tag, status, streamed, duration_ms, ttft_ms,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, error`

func scanSummary(row interface{ Scan(...any) error }, c *Call, extra ...any) error {
	dest := []any{&c.ID, &c.StartedAt, &c.Provider, &c.Method, &c.Path, &c.Model, &c.Tag, &c.Status,
		&c.Streamed, &c.DurationMS, &c.TTFTMS, &c.InputTokens, &c.OutputTokens, &c.CacheRead,
		&c.CacheWrite, &c.CostUSD, &c.Error}
	return row.Scan(append(dest, extra...)...)
}

// List returns recent calls, newest first. q filters on model, tag or path;
// afterID returns only calls newer than that id (for polling).
func (s *Store) List(limit int, q string, afterID int64) ([]Call, error) {
	where := []string{"id > ?"}
	args := []any{afterID}
	if q = strings.TrimSpace(q); q != "" {
		where = append(where, "(model LIKE ? OR tag LIKE ? OR path LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT `+summaryCols+` FROM calls WHERE `+strings.Join(where, " AND ")+
		` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		if err := scanSummary(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Get(id int64) (*Call, error) {
	var c Call
	err := scanSummary(s.db.QueryRow(`SELECT `+summaryCols+`,
		request_headers, request_body, response_headers, response_body, response_text
		FROM calls WHERE id = ?`, id), &c,
		&c.RequestHeaders, &c.RequestBody, &c.ResponseHeaders, &c.ResponseBody, &c.ResponseText)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

type Stats struct {
	Calls         int64   `json:"calls"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	UnpricedCalls int64   `json:"unpriced_calls"`
}

// StatsSince totals calls started at or after t.
func (s *Store) StatsSince(t time.Time) (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(input_tokens + cache_read_tokens + cache_write_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cost_usd), 0),
		COALESCE(SUM(CASE WHEN cost_usd IS NULL AND status < 400 THEN 1 ELSE 0 END), 0)
		FROM calls WHERE started_at >= ?`, t.UTC().Format(timeFormat)).
		Scan(&st.Calls, &st.InputTokens, &st.OutputTokens, &st.CostUSD, &st.UnpricedCalls)
	return st, err
}

func (s *Store) DeleteAll() error {
	_, err := s.db.Exec(`DELETE FROM calls`)
	return err
}
