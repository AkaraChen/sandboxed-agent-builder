// Package domain holds the builder's entities and the rules that hold for
// them whatever stores or executes builds: attempt states and their
// transitions, failures and artifacts. It imports nothing outside the
// standard library.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// Status is the state of one attempt.
//
//	queued → running → succeeded | failed | cancelled | interrupted
//	queued → cancelled (cancelled before any worker claimed it)
type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"
)

// Terminal reports whether no further transition leaves s.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Active reports whether an attempt in s may still produce a result.
func (s Status) Active() bool { return s == StatusQueued || s == StatusRunning }

// CanFinishAs reports whether a running attempt may end in to. Only a
// running attempt ends; a queued one is cancelled by CancelQueued.
func (s Status) CanFinishAs(to Status) bool {
	return s == StatusRunning && to.Terminal()
}

// Stage is where an attempt failed.
type Stage string

const (
	StagePlan      Stage = "plan"
	StageBuild     Stage = "build"
	StagePublish   Stage = "publish"
	StageReconcile Stage = "reconcile"
)

// Failure explains why an attempt did not succeed.
type Failure struct {
	Stage   Stage  `json:"stage"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Failure codes.
const (
	CodePlanFailed       = "plan_failed"
	CodeBuildFailed      = "build_failed"
	CodePublishFailed    = "publish_failed"
	CodeCancelled        = "cancelled"
	CodeInterrupted      = "interrupted"
	CodeLeaseLost        = "lease_lost"
	CodeExecutorError    = "executor_error"
	CodeArtifactNotFound = "artifact_not_found"
)

// Artifact is where an attempt's image is. ImageID and DaemonRef name it in
// the builder's Docker daemon; the registry fields are set only once a push
// was confirmed. An image ID is not a manifest digest.
type Artifact struct {
	ImageID     string `json:"imageId"`
	DaemonRef   string `json:"daemonRef"`
	Platform    string `json:"platform"`
	Repository  string `json:"repository,omitempty"`
	Digest      string `json:"digest,omitempty"`
	RegistryRef string `json:"registryRef,omitempty"`
}

// Build is one submission: an immutable spec snapshot and its attempts.
type Build struct {
	ID             string
	Caller         string
	IdempotencyKey string
	// RequestHash binds the idempotency key to the request's content.
	RequestHash string
	// SpecDigest identifies the resolved spec; equal digests may share
	// caches but are still separate builds.
	SpecDigest   string
	RawSpec      []byte
	ResolvedSpec []byte
	NoCache      bool
	CreatedAt    time.Time
	// CurrentAttemptID is the newest attempt.
	CurrentAttemptID string
	Attempts         int
}

// Attempt is one execution of a build.
type Attempt struct {
	ID        string
	BuildID   string
	Number    int
	Status    Status
	CreatedAt time.Time
	StartedAt *time.Time
	EndedAt   *time.Time
	// WorkerID, Generation and LeaseExpiresAt are the current claim. Each
	// claim increments Generation; writes carrying an older one are stale.
	WorkerID          string
	Generation        int64
	LeaseExpiresAt    *time.Time
	CancelRequestedAt *time.Time
	Failure           *Failure
	Artifact          *Artifact
	LogsTruncated     bool
	// Recovered is set when reconciliation, not the worker that ran the
	// attempt, recorded its outcome.
	Recovered bool
}

// Lease is a worker's right to write an attempt's progress.
type Lease struct {
	AttemptID  string
	WorkerID   string
	Generation int64
}

// LogEntry is one line of an attempt's log.
type LogEntry struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	Stage  Stage     `json:"stage"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

// Outcome is how a running attempt ended.
type Outcome struct {
	Status   Status
	Failure  *Failure
	Artifact *Artifact
}

// Validate checks an outcome is a legal end for a running attempt.
func (o Outcome) Validate() error {
	if !StatusRunning.CanFinishAs(o.Status) {
		return fmt.Errorf("%w: running → %s", ErrInvalidTransition, o.Status)
	}
	if o.Status == StatusSucceeded && (o.Artifact == nil || o.Artifact.ImageID == "") {
		return fmt.Errorf("%w: success needs an artifact", ErrInvalidTransition)
	}
	if o.Status != StatusSucceeded && o.Failure == nil {
		return fmt.Errorf("%w: %s needs a failure", ErrInvalidTransition, o.Status)
	}
	return nil
}

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different content")
	ErrActiveAttempt       = errors.New("build has an active attempt")
	ErrNotActive           = errors.New("attempt is not active")
	ErrStaleLease          = errors.New("lease is stale")
	ErrInvalidTransition   = errors.New("invalid status transition")
)

// SpecError is a rejected build spec.
type SpecError struct{ Problems []string }

func (e *SpecError) Error() string {
	return fmt.Sprintf("invalid spec: %v", e.Problems)
}
