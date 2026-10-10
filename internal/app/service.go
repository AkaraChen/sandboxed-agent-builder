package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

// Service is the API's side of the builder.
type Service struct {
	Store    Store
	Resolver Resolver
	Policy   func() spec.Policy
	Clock    Clock
	IDs      IDs
	Log      *slog.Logger
}

// ErrResolve wraps failures of the resolver's upstreams (registries, npm,
// git), which are not the caller's fault.
var ErrResolve = errors.New("resolving the spec failed")

// Submitted is the result of Submit.
type Submitted struct {
	Build    domain.Build
	Attempt  domain.Attempt
	Replayed bool
}

// Submit validates and resolves a build request and stores it with its
// first, queued attempt. The idempotency key is checked first: replaying a
// key returns the stored build without resolving anything again.
func (s *Service) Submit(ctx context.Context, caller, key string, body []byte) (Submitted, error) {
	var req spec.Request
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return Submitted{}, &domain.SpecError{Problems: []string{"body: " + err.Error()}}
	}
	if d.More() {
		return Submitted{}, &domain.SpecError{Problems: []string{"body: trailing data"}}
	}
	hash, err := spec.Hash(req)
	if err != nil {
		return Submitted{}, err
	}

	if existing, err := s.Store.FindBuildByKey(ctx, caller, key); err == nil {
		if existing.RequestHash != hash {
			return Submitted{}, domain.ErrIdempotencyConflict
		}
		a, err := s.Store.GetAttempt(ctx, existing.ID, existing.CurrentAttemptID)
		return Submitted{Build: existing, Attempt: a, Replayed: true}, err
	} else if !errors.Is(err, domain.ErrNotFound) {
		return Submitted{}, err
	}

	if err := req.Spec.Normalize(s.Policy()); err != nil {
		return Submitted{}, err
	}
	resolved, err := s.Resolver.Resolve(ctx, req.Spec)
	if err != nil {
		var se *domain.SpecError
		if errors.As(err, &se) {
			return Submitted{}, err
		}
		return Submitted{}, fmt.Errorf("%w: %v", ErrResolve, err)
	}
	resolvedJSON, err := json.Marshal(resolved)
	if err != nil {
		return Submitted{}, err
	}

	now := s.Clock.Now().UTC()
	b := domain.Build{
		ID: s.IDs.New("bld"), Caller: caller, IdempotencyKey: key, RequestHash: hash,
		SpecDigest: resolved.Digest(), RawSpec: body, ResolvedSpec: resolvedJSON,
		NoCache: req.NoCache, CreatedAt: now,
	}
	a := domain.Attempt{ID: s.IDs.New("att"), BuildID: b.ID, Number: 1, Status: domain.StatusQueued, CreatedAt: now}
	b.CurrentAttemptID, b.Attempts = a.ID, 1

	// Once resolved, storing completes even if the client went away: the
	// build exists from here on and a replay of the key finds it.
	stored, created, err := s.Store.CreateBuild(context.WithoutCancel(ctx), NewBuild{Build: b, Attempt: a})
	if err != nil {
		return Submitted{}, err
	}
	if !created {
		if stored.RequestHash != hash {
			return Submitted{}, domain.ErrIdempotencyConflict
		}
		a, err = s.Store.GetAttempt(ctx, stored.ID, stored.CurrentAttemptID)
		return Submitted{Build: stored, Attempt: a, Replayed: true}, err
	}
	s.Log.InfoContext(ctx, "build submitted", "build_id", b.ID, "attempt_id", a.ID, "caller", caller, "spec_digest", b.SpecDigest)
	return Submitted{Build: stored, Attempt: a}, nil
}

// View is a build with its current attempt.
type View struct {
	Build   domain.Build
	Current domain.Attempt
}

func (s *Service) Get(ctx context.Context, caller, id string) (View, error) {
	b, err := s.Store.GetBuild(ctx, caller, id)
	if err != nil {
		return View{}, err
	}
	a, err := s.Store.GetAttempt(ctx, b.ID, b.CurrentAttemptID)
	return View{Build: b, Current: a}, err
}

func (s *Service) Attempts(ctx context.Context, caller, id string, p Page) ([]domain.Attempt, string, error) {
	if _, err := s.Store.GetBuild(ctx, caller, id); err != nil {
		return nil, "", err
	}
	return s.Store.ListAttempts(ctx, id, p)
}

// Logs reads an attempt's log lines after seq, the current attempt's when
// attemptID is empty.
func (s *Service) Logs(ctx context.Context, caller, id, attemptID string, after int64, limit int) (domain.Attempt, []domain.LogEntry, error) {
	b, err := s.Store.GetBuild(ctx, caller, id)
	if err != nil {
		return domain.Attempt{}, nil, err
	}
	if attemptID == "" {
		attemptID = b.CurrentAttemptID
	}
	a, err := s.Store.GetAttempt(ctx, b.ID, attemptID)
	if err != nil {
		return domain.Attempt{}, nil, err
	}
	entries, err := s.Store.ListLogs(ctx, a.ID, after, limit)
	return a, entries, err
}

// Cancel requests the current attempt to stop. The returned attempt shows
// the actual state: cancelled only once nothing runs any more.
func (s *Service) Cancel(ctx context.Context, caller, id string) (domain.Attempt, error) {
	if _, err := s.Store.GetBuild(ctx, caller, id); err != nil {
		return domain.Attempt{}, err
	}
	a, err := s.Store.RequestCancel(context.WithoutCancel(ctx), id, s.Clock.Now().UTC())
	if err == nil {
		s.Log.InfoContext(ctx, "cancel requested", "build_id", id, "attempt_id", a.ID, "status", a.Status)
	}
	return a, err
}

// Retry queues a new attempt of the same resolved spec; locked versions do
// not move. The idempotency key makes repeating the request safe.
func (s *Service) Retry(ctx context.Context, caller, id, key string) (domain.Attempt, bool, error) {
	b, err := s.Store.GetBuild(ctx, caller, id)
	if err != nil {
		return domain.Attempt{}, false, err
	}
	hash, err := spec.Hash(map[string]string{"retry": b.ID})
	if err != nil {
		return domain.Attempt{}, false, err
	}
	a := domain.Attempt{ID: s.IDs.New("att"), BuildID: b.ID, Status: domain.StatusQueued, CreatedAt: s.Clock.Now().UTC()}
	a, created, err := s.Store.CreateRetry(context.WithoutCancel(ctx), caller, key, hash, a)
	if err == nil && created {
		s.Log.InfoContext(ctx, "retry queued", "build_id", b.ID, "attempt_id", a.ID, "attempt", a.Number)
	}
	return a, created, err
}
