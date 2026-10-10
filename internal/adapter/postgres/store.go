// Package postgres is the Store on PostgreSQL (pgx), with goose migrations.
//
// Claiming takes the oldest matching attempt with FOR UPDATE SKIP LOCKED,
// so any number of builder instances share one queue. Lease writes are
// conditioned on (attempt, worker, generation): a claim or takeover
// increments the generation, fencing out the old holder.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ pool *pgxpool.Pool }

var _ app.Store = (*Store)(nil)

// Open connects to url and applies pending migrations; concurrent
// instances wait on goose's Postgres session lock.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		// Times leave the store in UTC, as the API reports them.
		conn.TypeMap().RegisterType(&pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID, Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC}})
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Migrate applies the embedded goose migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, dir, goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}

func (s *Store) Close() { s.pool.Close() }

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) tx(ctx context.Context, f func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, f)
}

const buildCols = `id, caller, idempotency_key, request_hash, spec_digest, raw_spec, resolved_spec, no_cache, created_at, current_attempt_id, attempts`

func scanBuild(row pgx.Row) (domain.Build, error) {
	var b domain.Build
	err := row.Scan(&b.ID, &b.Caller, &b.IdempotencyKey, &b.RequestHash, &b.SpecDigest, &b.RawSpec, &b.ResolvedSpec, &b.NoCache, &b.CreatedAt, &b.CurrentAttemptID, &b.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, domain.ErrNotFound
	}
	return b, err
}

const attemptCols = `id, build_id, number, status, created_at, started_at, ended_at, coalesce(worker_id, ''), generation, lease_expires_at, cancel_requested_at, failure, artifact, recovered, logs_truncated`

func scanAttempt(row pgx.Row) (domain.Attempt, error) {
	var a domain.Attempt
	err := row.Scan(&a.ID, &a.BuildID, &a.Number, &a.Status, &a.CreatedAt, &a.StartedAt, &a.EndedAt, &a.WorkerID, &a.Generation,
		&a.LeaseExpiresAt, &a.CancelRequestedAt, &a.Failure, &a.Artifact, &a.Recovered, &a.LogsTruncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, domain.ErrNotFound
	}
	return a, err
}

func getAttempt(ctx context.Context, tx pgx.Tx, id string) (domain.Attempt, error) {
	return scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptCols+` FROM attempts WHERE id = $1`, id))
}

func (s *Store) FindBuildByKey(ctx context.Context, caller, key string) (domain.Build, error) {
	return scanBuild(s.pool.QueryRow(ctx, `SELECT `+buildCols+` FROM builds WHERE caller = $1 AND idempotency_key = $2`, caller, key))
}

func (s *Store) CreateBuild(ctx context.Context, nb app.NewBuild) (domain.Build, bool, error) {
	b, a := nb.Build, nb.Attempt
	b.CurrentAttemptID, b.Attempts = a.ID, 1
	created := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO builds (`+buildCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1)
			ON CONFLICT (caller, idempotency_key) DO NOTHING`,
			b.ID, b.Caller, b.IdempotencyKey, b.RequestHash, b.SpecDigest, b.RawSpec, b.ResolvedSpec, b.NoCache, b.CreatedAt, a.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Another request stored this key first.
			b, err = scanBuild(tx.QueryRow(ctx, `SELECT `+buildCols+` FROM builds WHERE caller = $1 AND idempotency_key = $2`, b.Caller, b.IdempotencyKey))
			return err
		}
		created = true
		_, err = tx.Exec(ctx, `INSERT INTO attempts (id, build_id, number, status, created_at) VALUES ($1, $2, 1, 'queued', $3)`, a.ID, b.ID, a.CreatedAt)
		return err
	})
	return b, created, err
}

func (s *Store) GetBuild(ctx context.Context, caller, id string) (domain.Build, error) {
	return scanBuild(s.pool.QueryRow(ctx, `SELECT `+buildCols+` FROM builds WHERE id = $1 AND caller = $2`, id, caller))
}

func (s *Store) GetAttempt(ctx context.Context, buildID, attemptID string) (domain.Attempt, error) {
	return scanAttempt(s.pool.QueryRow(ctx, `SELECT `+attemptCols+` FROM attempts WHERE id = $1 AND build_id = $2`, attemptID, buildID))
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
	rows, _ := s.pool.Query(ctx, `SELECT `+attemptCols+` FROM attempts WHERE build_id = $1 AND number > $2 ORDER BY number LIMIT $3`, buildID, after, p.Limit+1)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Attempt, error) { return scanAttempt(r) })
	if err != nil {
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
	rows, _ := s.pool.Query(ctx, `SELECT seq, time, stage, stream, text FROM attempt_logs WHERE attempt_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, attemptID, after, limit)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[domain.LogEntry])
	if out == nil {
		out = []domain.LogEntry{}
	}
	return out, err
}

func (s *Store) RequestCancel(ctx context.Context, buildID string, now time.Time) (domain.Attempt, error) {
	var out domain.Attempt
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var current string
		if err := tx.QueryRow(ctx, `SELECT current_attempt_id FROM builds WHERE id = $1 FOR UPDATE`, buildID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		failure := domain.Failure{Stage: domain.StagePlan, Code: domain.CodeCancelled, Message: "cancelled before a worker started it"}
		if _, err := tx.Exec(ctx, `UPDATE attempts SET status = 'cancelled', cancel_requested_at = $1, ended_at = $1, failure = $2
			WHERE id = $3 AND status = 'queued'`, now, failure, current); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE attempts SET cancel_requested_at = $1 WHERE id = $2 AND status = 'running' AND cancel_requested_at IS NULL`, now, current); err != nil {
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
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// Retries of one build are serialized on its row.
		var current string
		var count int
		if err := tx.QueryRow(ctx, `SELECT current_attempt_id, attempts FROM builds WHERE id = $1 FOR UPDATE`, a.BuildID).Scan(&current, &count); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		var prevHash, prevAttempt string
		err := tx.QueryRow(ctx, `SELECT request_hash, attempt_id FROM retries WHERE caller = $1 AND idempotency_key = $2`, caller, key).Scan(&prevHash, &prevAttempt)
		switch {
		case err == nil:
			if prevHash != hash {
				return domain.ErrIdempotencyConflict
			}
			out, err = getAttempt(ctx, tx, prevAttempt)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
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
		if _, err := tx.Exec(ctx, `INSERT INTO attempts (id, build_id, number, status, created_at) VALUES ($1, $2, $3, 'queued', $4)`,
			a.ID, a.BuildID, a.Number, a.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE builds SET current_attempt_id = $1, attempts = $2 WHERE id = $3`, a.ID, a.Number, a.BuildID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO retries (caller, idempotency_key, request_hash, build_id, attempt_id) VALUES ($1, $2, $3, $4, $5)`,
			caller, key, hash, a.BuildID, a.ID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				// The key was used for another build at the same time.
				return domain.ErrIdempotencyConflict
			}
			return err
		}
		out, err = getAttempt(ctx, tx, a.ID)
		created = true
		return err
	})
	return out, created, err
}

// Claim takes the oldest queued attempt for workerID.
func (s *Store) Claim(ctx context.Context, workerID string, now, until time.Time) (*app.Claim, error) {
	return s.claim(ctx, workerID, until, `status = 'queued'`, `status = 'running', started_at = $3, `, now)
}

// TakeOver takes the oldest running attempt whose lease expired.
func (s *Store) TakeOver(ctx context.Context, workerID string, now, until time.Time) (*app.Claim, error) {
	return s.claim(ctx, workerID, until, `status = 'running' AND lease_expires_at < $3`, ``, now)
}

// claim takes the oldest attempt matching where for workerID; SKIP LOCKED
// lets concurrent claimers pass each other, and the generation increment
// fences the previous holder. Exactly one of where and set uses $3, now.
func (s *Store) claim(ctx context.Context, workerID string, until time.Time, where, set string, now time.Time) (*app.Claim, error) {
	var out *app.Claim
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `UPDATE attempts SET `+set+`worker_id = $1, generation = generation + 1, lease_expires_at = $2
			WHERE id = (SELECT id FROM attempts WHERE `+where+` ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING id`, workerID, until, now).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		a, err := getAttempt(ctx, tx, id)
		if err != nil {
			return err
		}
		b, err := scanBuild(tx.QueryRow(ctx, `SELECT `+buildCols+` FROM builds WHERE id = $1`, a.BuildID))
		out = &app.Claim{Build: b, Attempt: a, Lease: domain.Lease{AttemptID: a.ID, WorkerID: a.WorkerID, Generation: a.Generation}}
		return err
	})
	return out, err
}

const leaseCond = `id = $1 AND worker_id = $2 AND generation = $3 AND status = 'running'`

func (s *Store) Renew(ctx context.Context, l domain.Lease, until time.Time) (bool, error) {
	var cancel bool
	err := s.pool.QueryRow(ctx, `UPDATE attempts SET lease_expires_at = $4 WHERE `+leaseCond+` RETURNING cancel_requested_at IS NOT NULL`,
		l.AttemptID, l.WorkerID, l.Generation, until).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.ErrStaleLease
	}
	return cancel, err
}

const truncatedNote = "[log truncated: the attempt reached the builder's log size limit; later lines were dropped]"

func (s *Store) AppendLogs(ctx context.Context, l domain.Lease, entries []domain.LogEntry, maxBytes int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var seq, size int64
		var truncated bool
		err := tx.QueryRow(ctx, `SELECT log_seq, log_bytes, logs_truncated FROM attempts WHERE `+leaseCond+` FOR UPDATE`,
			l.AttemptID, l.WorkerID, l.Generation).Scan(&seq, &size, &truncated)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrStaleLease
		}
		if err != nil || truncated {
			return err
		}
		var rows [][]any
		for _, e := range entries {
			// PostgreSQL text holds neither NUL nor invalid UTF-8.
			text := strings.ToValidUTF8(strings.ReplaceAll(e.Text, "\x00", ""), "�")
			stream := e.Stream
			if maxBytes > 0 && size+int64(len(text)) > maxBytes {
				truncated, text, stream = true, truncatedNote, "builder"
			}
			seq++
			size += int64(len(text))
			rows = append(rows, []any{l.AttemptID, seq, e.Time, string(e.Stage), stream, text})
			if truncated {
				break
			}
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"attempt_logs"}, []string{"attempt_id", "seq", "time", "stage", "stream", "text"}, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE attempts SET log_seq = $1, log_bytes = $2, logs_truncated = $3 WHERE id = $4`, seq, size, truncated, l.AttemptID)
		return err
	})
}

func (s *Store) Finish(ctx context.Context, l domain.Lease, o domain.Outcome, now time.Time, recovered bool) error {
	if err := o.Validate(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE attempts SET status = $4, ended_at = $5, lease_expires_at = NULL, failure = $6, artifact = $7, recovered = $8
		WHERE `+leaseCond, l.AttemptID, l.WorkerID, l.Generation, string(o.Status), now, o.Failure, o.Artifact, recovered)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrStaleLease
	}
	return nil
}
