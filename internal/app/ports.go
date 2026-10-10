// Package app holds the builder's use cases: submitting, reading,
// cancelling and retrying builds, the worker that runs attempts and the
// reconciler that settles attempts whose worker went away. It depends on
// the ports below, never on a database, Docker or HTTP.
package app

import (
	"context"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

// NewBuild is a build to create with its first attempt.
type NewBuild struct {
	Build   domain.Build
	Attempt domain.Attempt
}

// Page is a cursor page request; Cursor is opaque to callers.
type Page struct {
	Limit  int
	Cursor string
}

// Claim is a running attempt a worker holds.
type Claim struct {
	Build   domain.Build
	Attempt domain.Attempt
	Lease   domain.Lease
}

// Store persists builds, attempts and logs. Writes that carry a lease fail
// with domain.ErrStaleLease once another claim superseded it.
type Store interface {
	// CreateBuild stores b, or returns the build already stored under the
	// same caller and idempotency key (created false). A stored build with
	// a different request hash is domain.ErrIdempotencyConflict.
	CreateBuild(ctx context.Context, b NewBuild) (domain.Build, bool, error)
	FindBuildByKey(ctx context.Context, caller, key string) (domain.Build, error)
	GetBuild(ctx context.Context, caller, id string) (domain.Build, error)
	GetAttempt(ctx context.Context, buildID, attemptID string) (domain.Attempt, error)
	ListAttempts(ctx context.Context, buildID string, p Page) ([]domain.Attempt, string, error)
	ListLogs(ctx context.Context, attemptID string, after int64, limit int) ([]domain.LogEntry, error)

	// RequestCancel records a cancel request on the build's current
	// attempt. A queued attempt is cancelled at once; a running one keeps
	// running until its worker confirms it stopped. Terminal attempts are
	// returned unchanged.
	RequestCancel(ctx context.Context, buildID string, now time.Time) (domain.Attempt, error)
	// CreateRetry adds attempt a to its build unless the build's current
	// attempt is active (domain.ErrActiveAttempt). The same caller key and
	// hash returns the attempt made before (created false).
	CreateRetry(ctx context.Context, caller, key, hash string, a domain.Attempt) (domain.Attempt, bool, error)

	// Claim takes the oldest queued attempt for workerID, or returns nil.
	Claim(ctx context.Context, workerID string, now, until time.Time) (*Claim, error)
	// Renew extends a lease and reports whether a cancel was requested.
	Renew(ctx context.Context, l domain.Lease, until time.Time) (cancel bool, err error)
	AppendLogs(ctx context.Context, l domain.Lease, entries []domain.LogEntry, maxBytes int64) error
	Finish(ctx context.Context, l domain.Lease, o domain.Outcome, now time.Time, recovered bool) error

	// Expired lists running attempts whose lease ended before now.
	Expired(ctx context.Context, now time.Time, limit int) ([]domain.Attempt, error)
	// TakeOver claims an expired running attempt for reconciliation,
	// fencing out its old worker.
	TakeOver(ctx context.Context, attemptID, workerID string, now, until time.Time) (*Claim, error)
}

// Resolver locks a normalized spec: digests, exact versions, commits.
type Resolver interface {
	Resolve(ctx context.Context, s spec.Spec) (spec.Resolved, error)
}

// Planner writes the build context for an attempt.
type Planner interface {
	Plan(ctx context.Context, r spec.Resolved, dir string) (PlanResult, error)
}

type PlanResult struct {
	Dir     string
	Secrets []string
}

// LogFunc receives build output lines.
type LogFunc func(stream, text string)

// BuildRequest is one attempt's image build.
type BuildRequest struct {
	BuildID    string
	AttemptID  string
	SpecDigest string
	Context    string
	Platform   string
	// Tag is the attempt's own daemon reference; reconciliation finds the
	// attempt's image by it and by its labels.
	Tag     string
	Secrets map[string][]byte
	NoCache bool
}

// Image is a built image in the daemon.
type Image struct {
	ID       string
	Ref      string
	Platform string
}

// Executor builds images in a Docker daemon.
type Executor interface {
	// Build runs the build until it ends or ctx is done. It returns only
	// once the daemon stopped working on it, so a cancelled ctx returning
	// means the build stopped.
	Build(ctx context.Context, req BuildRequest, log LogFunc) (Image, error)
	// FindAttemptImage returns the image tagged for the attempt, if the
	// daemon has it and its labels name the attempt.
	FindAttemptImage(ctx context.Context, buildID, attemptID string) (*Image, error)
}

// Published is an image confirmed in a registry.
type Published struct {
	Repository string
	Digest     string
	Ref        string
}

// Publisher pushes images to the configured registry.
type Publisher interface {
	Publish(ctx context.Context, img Image, repository, tag string, log LogFunc) (Published, error)
	// Find returns the manifest under repository:tag, if there is one.
	Find(ctx context.Context, repository, tag string) (*Published, error)
}

// Secrets gives build secret values by id.
type Secrets interface {
	Get(id string) ([]byte, error)
	// Values lists every configured secret value, for redaction.
	Values() [][]byte
}

// Clock and IDs make time and identifiers injectable in tests.
type Clock interface{ Now() time.Time }

type IDs interface {
	New(prefix string) string
}
