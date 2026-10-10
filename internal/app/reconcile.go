package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

// Reconciler settles running attempts whose worker stopped renewing its
// lease (a crash, a restart, a lost database connection). A Docker build
// does not continue across builder restarts, so the only question is
// whether the attempt's result exists: the attempt's own tag and labels in
// the daemon, and in registry mode its manifest in the registry. A
// confirmed result is recorded as recovered success; anything else is
// interrupted and waits for an explicit retry.
type Reconciler struct {
	Store     Store
	Executor  Executor
	Publisher Publisher
	ID        string
	Lease     time.Duration
	Log       *slog.Logger
}

// Run reconciles now and then every interval until ctx is done.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	for {
		if n, err := r.RunOnce(ctx); err != nil {
			r.Log.Error("reconciling attempts failed", "err", err)
		} else if n > 0 {
			r.Log.Info("reconciled attempts", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// RunOnce settles the attempts expired now and returns how many it settled.
func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	expired, err := r.Store.Expired(ctx, now, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range expired {
		c, err := r.Store.TakeOver(ctx, a.ID, r.ID, now, now.Add(r.Lease))
		if err != nil {
			r.Log.Warn("taking over an attempt failed", "attempt_id", a.ID, "err", err)
			continue
		}
		if c == nil {
			continue
		}
		if r.settle(ctx, c) {
			n++
		}
	}
	return n, nil
}

func (r *Reconciler) settle(ctx context.Context, c *Claim) bool {
	log := r.Log.With("build_id", c.Build.ID, "attempt_id", c.Attempt.ID, "generation", c.Lease.Generation)
	var rs spec.Resolved
	_ = json.Unmarshal(c.Build.ResolvedSpec, &rs)

	img, err := r.Executor.FindAttemptImage(ctx, c.Build.ID, c.Attempt.ID)
	if err != nil {
		// The daemon cannot answer; the lease runs out and a later round
		// tries again. Nothing is guessed.
		log.Warn("checking the daemon for the attempt's image failed", "err", err)
		return false
	}
	var art *domain.Artifact
	if img != nil {
		art = &domain.Artifact{ImageID: img.ID, DaemonRef: img.Ref, Platform: rs.Platform}
	}
	outcome := domain.Outcome{Status: domain.StatusInterrupted, Artifact: art, Failure: &domain.Failure{
		Stage: domain.StageReconcile, Code: domain.CodeInterrupted,
		Message: "the worker stopped before the result was recorded and the daemon has no image for this attempt; retry to build again (the layer cache is reused)",
	}}
	switch {
	case rs.Output.Type == spec.OutputRegistry:
		var pub *Published
		if r.Publisher != nil {
			pub, err = r.Publisher.Find(ctx, rs.Output.Repository, c.Attempt.ID)
			if err != nil {
				log.Warn("checking the registry for the attempt's manifest failed", "err", err)
				return false
			}
		}
		if pub != nil && art != nil {
			art.Repository, art.Digest, art.RegistryRef = pub.Repository, pub.Digest, pub.Ref
			outcome = domain.Outcome{Status: domain.StatusSucceeded, Artifact: art}
		} else if art != nil {
			outcome.Failure.Stage = domain.StagePublish
			outcome.Failure.Message = "the image was built but publishing to the registry was not confirmed; retry to build and publish again"
		}
	case art != nil:
		outcome = domain.Outcome{Status: domain.StatusSucceeded, Artifact: art}
	}
	if err := r.Store.Finish(ctx, c.Lease, outcome, time.Now().UTC(), true); err != nil {
		log.Error("recording the reconciled outcome failed", "err", err)
		return false
	}
	log.Info("attempt reconciled", "status", outcome.Status)
	return true
}
