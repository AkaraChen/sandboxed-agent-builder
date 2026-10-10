// Package sqlite is the Store on SQLite (modernc.org/sqlite, no cgo).
//
// Every write transaction starts with BEGIN IMMEDIATE, which takes SQLite's
// single write lock up front, so claiming a queued attempt is a serialized
// read-then-update even across processes sharing the database file. Lease
// writes are conditioned on (attempt, worker, generation): a claim or
// takeover increments the generation, fencing out the old holder.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ db *sql.DB }

var _ app.Store = (*Store)(nil)

// Open opens (creating) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(15000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil || n > 0 {
				return err
			}
			body, err := migrations.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", base, err)
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, version, base, time.Now().UnixNano())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const buildCols = `id, caller, idempotency_key, request_hash, spec_digest, raw_spec, resolved_spec, no_cache, created_at, current_attempt_id, attempts`

func scanBuild(row interface{ Scan(...any) error }) (domain.Build, error) {
	var b domain.Build
	var created int64
	err := row.Scan(&b.ID, &b.Caller, &b.IdempotencyKey, &b.RequestHash, &b.SpecDigest, &b.RawSpec, &b.ResolvedSpec, &b.NoCache, &created, &b.CurrentAttemptID, &b.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return b, domain.ErrNotFound
	}
	b.CreatedAt = fromNanos(created)
	return b, err
}

const attemptCols = `id, build_id, number, status, created_at, started_at, ended_at, worker_id, generation, lease_expires_at, cancel_requested_at, failure_stage, failure_code, failure_message, artifact, recovered, logs_truncated`

func scanAttempt(row interface{ Scan(...any) error }) (domain.Attempt, error) {
	var a domain.Attempt
	var created int64
	var started, ended, lease, cancel sql.NullInt64
	var worker, stage, code, msg, artifact sql.NullString
	err := row.Scan(&a.ID, &a.BuildID, &a.Number, &a.Status, &created, &started, &ended, &worker, &a.Generation, &lease, &cancel, &stage, &code, &msg, &artifact, &a.Recovered, &a.LogsTruncated)
	if errors.Is(err, sql.ErrNoRows) {
		return a, domain.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.CreatedAt = fromNanos(created)
	a.StartedAt, a.EndedAt, a.LeaseExpiresAt, a.CancelRequestedAt = optTime(started), optTime(ended), optTime(lease), optTime(cancel)
	a.WorkerID = worker.String
	if stage.Valid {
		a.Failure = &domain.Failure{Stage: domain.Stage(stage.String), Code: code.String, Message: msg.String}
	}
	if artifact.Valid {
		a.Artifact = &domain.Artifact{}
		if err := json.Unmarshal([]byte(artifact.String), a.Artifact); err != nil {
			return a, err
		}
	}
	return a, nil
}

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func optTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}

func (s *Store) FindBuildByKey(ctx context.Context, caller, key string) (domain.Build, error) {
	return scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE caller = ? AND idempotency_key = ?`, caller, key))
}

func (s *Store) CreateBuild(ctx context.Context, nb app.NewBuild) (domain.Build, bool, error) {
	var out domain.Build
	created := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		existing, err := scanBuild(tx.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE caller = ? AND idempotency_key = ?`, nb.Build.Caller, nb.Build.IdempotencyKey))
		if err == nil {
			out = existing
			return nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		b, a := nb.Build, nb.Attempt
		if _, err := tx.ExecContext(ctx, `INSERT INTO builds (`+buildCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ID, b.Caller, b.IdempotencyKey, b.RequestHash, b.SpecDigest, b.RawSpec, b.ResolvedSpec, b.NoCache, b.CreatedAt.UnixNano(), a.ID, 1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (id, build_id, number, status, created_at) VALUES (?, ?, 1, 'queued', ?)`,
			a.ID, b.ID, a.CreatedAt.UnixNano()); err != nil {
			return err
		}
		out, created = b, true
		out.CurrentAttemptID, out.Attempts = a.ID, 1
		return nil
	})
	return out, created, err
}

func (s *Store) GetBuild(ctx context.Context, caller, id string) (domain.Build, error) {
	return scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE id = ? AND caller = ?`, id, caller))
}

func getAttempt(ctx context.Context, q querier, id string) (domain.Attempt, error) {
	return scanAttempt(q.QueryRowContext(ctx, `SELECT `+attemptCols+` FROM attempts WHERE id = ?`, id))
}

func (s *Store) GetAttempt(ctx context.Context, buildID, attemptID string) (domain.Attempt, error) {
	return scanAttempt(s.db.QueryRowContext(ctx, `SELECT `+attemptCols+` FROM attempts WHERE id = ? AND build_id = ?`, attemptID, buildID))
}

// ListAttempts pages attempts by number, oldest first; the cursor is the
// last number seen.
func (s *Store) ListAttempts(ctx context.Context, buildID string, p app.Page) ([]domain.Attempt, string, error) {
	if p.Limit <= 0 {
		p.Limit = 20
	}
	after := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(p.Cursor)
		if err != nil || n < 0 {
			return nil, "", &domain.SpecError{Problems: []string{"cursor: invalid"}}
		}
		after = n
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+attemptCols+` FROM attempts WHERE build_id = ? AND number > ? ORDER BY number LIMIT ?`, buildID, after, p.Limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > p.Limit {
		out = out[:p.Limit]
		next = strconv.Itoa(out[len(out)-1].Number)
	}
	return out, next, nil
}

func (s *Store) ListLogs(ctx context.Context, attemptID string, after int64, limit int) ([]domain.LogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, time, stage, stream, text FROM attempt_logs WHERE attempt_id = ? AND seq > ? ORDER BY seq LIMIT ?`, attemptID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.LogEntry{}
	for rows.Next() {
		var e domain.LogEntry
		var t int64
		if err := rows.Scan(&e.Seq, &t, &e.Stage, &e.Stream, &e.Text); err != nil {
			return nil, err
		}
		e.Time = fromNanos(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) RequestCancel(ctx context.Context, buildID string, now time.Time) (domain.Attempt, error) {
	var out domain.Attempt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRowContext(ctx, `SELECT current_attempt_id FROM builds WHERE id = ?`, buildID).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		n := now.UnixNano()
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'cancelled', cancel_requested_at = ?, ended_at = ?,
			failure_stage = 'plan', failure_code = ?, failure_message = 'cancelled before a worker started it'
			WHERE id = ? AND status = 'queued'`, n, n, domain.CodeCancelled, current); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET cancel_requested_at = ? WHERE id = ? AND status = 'running' AND cancel_requested_at IS NULL`, n, current); err != nil {
			return err
		}
		var err error
		out, err = getAttempt(ctx, tx, current)
		return err
	})
	return out, err
}

func (s *Store) CreateRetry(ctx context.Context, caller, key, hash string, a domain.Attempt) (domain.Attempt, bool, error) {
	var out domain.Attempt
	created := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var prevHash, prevAttempt string
		err := tx.QueryRowContext(ctx, `SELECT request_hash, attempt_id FROM retries WHERE caller = ? AND idempotency_key = ?`, caller, key).Scan(&prevHash, &prevAttempt)
		switch {
		case err == nil:
			if prevHash != hash {
				return domain.ErrIdempotencyConflict
			}
			out, err = getAttempt(ctx, tx, prevAttempt)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var current string
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT current_attempt_id, attempts FROM builds WHERE id = ?`, a.BuildID).Scan(&current, &count); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		cur, err := getAttempt(ctx, tx, current)
		if err != nil {
			return err
		}
		if cur.Status.Active() {
			return domain.ErrActiveAttempt
		}
		a.Number = count + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (id, build_id, number, status, created_at) VALUES (?, ?, ?, 'queued', ?)`,
			a.ID, a.BuildID, a.Number, a.CreatedAt.UnixNano()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE builds SET current_attempt_id = ?, attempts = ? WHERE id = ?`, a.ID, a.Number, a.BuildID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO retries (caller, idempotency_key, request_hash, build_id, attempt_id) VALUES (?, ?, ?, ?, ?)`,
			caller, key, hash, a.BuildID, a.ID); err != nil {
			return err
		}
		out, err = getAttempt(ctx, tx, a.ID)
		created = true
		return err
	})
	return out, created, err
}

func (s *Store) loadClaim(ctx context.Context, q querier, attemptID string) (*app.Claim, error) {
	a, err := getAttempt(ctx, q, attemptID)
	if err != nil {
		return nil, err
	}
	b, err := scanBuild(q.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE id = ?`, a.BuildID))
	if err != nil {
		return nil, err
	}
	return &app.Claim{Build: b, Attempt: a, Lease: domain.Lease{AttemptID: a.ID, WorkerID: a.WorkerID, Generation: a.Generation}}, nil
}

func (s *Store) Claim(ctx context.Context, workerID string, now, until time.Time) (*app.Claim, error) {
	var out *app.Claim
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM attempts WHERE status = 'queued' ORDER BY created_at, id LIMIT 1`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'running', worker_id = ?, generation = generation + 1, lease_expires_at = ?, started_at = ?
			WHERE id = ? AND status = 'queued'`, workerID, until.UnixNano(), now.UnixNano(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		out, err = s.loadClaim(ctx, tx, id)
		return err
	})
	return out, err
}

const leaseCond = `id = ? AND worker_id = ? AND generation = ? AND status = 'running'`

func leaseArgs(l domain.Lease) []any { return []any{l.AttemptID, l.WorkerID, l.Generation} }

func (s *Store) Renew(ctx context.Context, l domain.Lease, until time.Time) (bool, error) {
	var cancel sql.NullInt64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE attempts SET lease_expires_at = ? WHERE `+leaseCond, append([]any{until.UnixNano()}, leaseArgs(l)...)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return domain.ErrStaleLease
		}
		return tx.QueryRowContext(ctx, `SELECT cancel_requested_at FROM attempts WHERE id = ?`, l.AttemptID).Scan(&cancel)
	})
	return cancel.Valid, err
}

const truncatedNote = "[log truncated: the attempt reached the builder's log size limit; later lines were dropped]"

func (s *Store) AppendLogs(ctx context.Context, l domain.Lease, entries []domain.LogEntry, maxBytes int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var seq, size int64
		var truncated bool
		err := tx.QueryRowContext(ctx, `SELECT log_seq, log_bytes, logs_truncated FROM attempts WHERE `+leaseCond, leaseArgs(l)...).Scan(&seq, &size, &truncated)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrStaleLease
		}
		if err != nil || truncated {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO attempt_logs (attempt_id, seq, time, stage, stream, text) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range entries {
			if maxBytes > 0 && size+int64(len(e.Text)) > maxBytes {
				truncated = true
				e = domain.LogEntry{Time: e.Time, Stage: e.Stage, Stream: "builder", Text: truncatedNote}
			}
			seq++
			size += int64(len(e.Text))
			if _, err := stmt.ExecContext(ctx, l.AttemptID, seq, e.Time.UnixNano(), string(e.Stage), e.Stream, e.Text); err != nil {
				return err
			}
			if truncated {
				break
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE attempts SET log_seq = ?, log_bytes = ?, logs_truncated = ? WHERE id = ?`, seq, size, truncated, l.AttemptID)
		return err
	})
}

func (s *Store) Finish(ctx context.Context, l domain.Lease, o domain.Outcome, now time.Time, recovered bool) error {
	if err := o.Validate(); err != nil {
		return err
	}
	var stage, code, msg, artifact sql.NullString
	if o.Failure != nil {
		stage = sql.NullString{String: string(o.Failure.Stage), Valid: true}
		code = sql.NullString{String: o.Failure.Code, Valid: true}
		msg = sql.NullString{String: o.Failure.Message, Valid: true}
	}
	if o.Artifact != nil {
		b, err := json.Marshal(o.Artifact)
		if err != nil {
			return err
		}
		artifact = sql.NullString{String: string(b), Valid: true}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE attempts SET status = ?, ended_at = ?, lease_expires_at = NULL,
			failure_stage = ?, failure_code = ?, failure_message = ?, artifact = ?, recovered = ? WHERE `+leaseCond,
			append([]any{string(o.Status), now.UnixNano(), stage, code, msg, artifact, recovered}, leaseArgs(l)...)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return domain.ErrStaleLease
		}
		return nil
	})
}

func (s *Store) Expired(ctx context.Context, now time.Time, limit int) ([]domain.Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attemptCols+` FROM attempts WHERE status = 'running' AND lease_expires_at < ? ORDER BY lease_expires_at LIMIT ?`, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) TakeOver(ctx context.Context, attemptID, workerID string, now, until time.Time) (*app.Claim, error) {
	var out *app.Claim
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE attempts SET worker_id = ?, generation = generation + 1, lease_expires_at = ?
			WHERE id = ? AND status = 'running' AND lease_expires_at < ?`, workerID, until.UnixNano(), attemptID, now.UnixNano())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		out, err = s.loadClaim(ctx, tx, attemptID)
		return err
	})
	return out, err
}
