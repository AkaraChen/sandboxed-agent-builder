package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

// WorkerConfig tunes a Worker.
type WorkerConfig struct {
	ID          string
	Concurrency int
	Poll        time.Duration
	Lease       time.Duration
	WorkDir     string
	MaxLogBytes int64
	// Grace is how long shutdown waits for running attempts before it
	// stops them and records them interrupted.
	Grace time.Duration
}

// Worker claims queued attempts and runs them.
type Worker struct {
	Store     Store
	Planner   Planner
	Executor  Executor
	Publisher Publisher // nil without a registry
	Secrets   Secrets
	Clock     Clock
	Config    WorkerConfig
	Log       *slog.Logger
}

var (
	errCancelRequested = errors.New("cancel requested")
	errLeaseLost       = errors.New("lease lost")
	errShutdown        = errors.New("builder shutting down")
)

// DaemonRef is the attempt's own tag in the daemon.
func DaemonRef(buildID, attemptID string) string {
	return "sandboxed-agent-builder/" + buildID + ":" + attemptID
}

// Run claims and runs attempts until ctx is done, then gives running
// attempts Config.Grace to finish before stopping them.
func (w *Worker) Run(ctx context.Context) {
	stop := make(chan struct{})
	work, cancelWork := context.WithCancelCause(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	for i := 0; i < max(1, w.Config.Concurrency); i++ {
		wg.Go(func() {
			for {
				ran, err := w.RunOnce(work)
				if err != nil {
					w.Log.Error("claiming an attempt failed", "err", err)
				}
				if ran {
					select {
					case <-stop:
						return
					default:
						continue
					}
				}
				select {
				case <-stop:
					return
				case <-time.After(w.Config.Poll):
				}
			}
		})
	}
	<-ctx.Done()
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.Config.Grace):
		cancelWork(errShutdown)
		<-done
	}
	cancelWork(nil)
}

// RunOnce claims one queued attempt and runs it to its end. It reports
// whether there was one.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	now := w.Clock.Now().UTC()
	claim, err := w.Store.Claim(ctx, w.Config.ID, now, now.Add(w.Config.Lease))
	if err != nil || claim == nil {
		return false, err
	}
	w.execute(ctx, claim)
	return true, nil
}

func (w *Worker) execute(parent context.Context, c *Claim) {
	log := w.Log.With("build_id", c.Build.ID, "attempt_id", c.Attempt.ID, "generation", c.Lease.Generation)
	ctx, span := otel.Tracer("sandboxed-agent-builder").Start(parent, "build.attempt")
	span.SetAttributes(
		attribute.String("build.id", c.Build.ID),
		attribute.String("build.attempt.id", c.Attempt.ID),
		attribute.Int("build.attempt.number", c.Attempt.Number),
		attribute.String("build.spec_digest", c.Build.SpecDigest),
	)
	defer span.End()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	sink := newLogSink(w.Store, c.Lease, w.Config.MaxLogBytes, redactor(w.Secrets), w.Clock, func(err error) {
		if errors.Is(err, domain.ErrStaleLease) {
			cancel(errLeaseLost)
		} else {
			log.Warn("writing build logs failed", "err", err)
		}
	})
	defer sink.close()

	// The lease is renewed while the attempt runs; a renewal also brings
	// in cancel requests.
	beat := make(chan struct{})
	go func() {
		defer close(beat)
		t := time.NewTicker(w.Config.Lease / 3)
		defer t.Stop()
		for {
			cancelled, err := w.Store.Renew(context.WithoutCancel(ctx), c.Lease, w.Clock.Now().UTC().Add(w.Config.Lease))
			switch {
			case errors.Is(err, domain.ErrStaleLease):
				cancel(errLeaseLost)
				return
			case err != nil:
				log.Warn("renewing the lease failed", "err", err)
			case cancelled:
				cancel(errCancelRequested)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	log.Info("attempt started")
	outcome := w.run(ctx, c, sink)
	cause := context.Cause(ctx)
	cancel(nil)
	<-beat
	sink.close()

	if errors.Is(cause, errLeaseLost) {
		log.Warn("lease lost; another claim owns the attempt, nothing recorded")
		span.SetStatus(codes.Error, "lease lost")
		return
	}
	if outcome.Status != domain.StatusSucceeded {
		switch {
		case errors.Is(cause, errCancelRequested):
			outcome = domain.Outcome{Status: domain.StatusCancelled, Artifact: outcome.Artifact, Failure: &domain.Failure{
				Stage: outcome.Failure.Stage, Code: domain.CodeCancelled, Message: "cancelled on request; the build stopped"}}
		case errors.Is(cause, errShutdown):
			outcome = domain.Outcome{Status: domain.StatusInterrupted, Artifact: outcome.Artifact, Failure: &domain.Failure{
				Stage: outcome.Failure.Stage, Code: domain.CodeInterrupted, Message: "the builder shut down while the attempt ran; retry to build again"}}
		}
	}
	if err := w.Store.Finish(context.WithoutCancel(ctx), c.Lease, outcome, w.Clock.Now().UTC(), false); err != nil {
		log.Error("recording the outcome failed", "err", err, "status", outcome.Status)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	attrs := []any{"status", outcome.Status}
	if outcome.Failure != nil {
		attrs = append(attrs, "stage", outcome.Failure.Stage, "code", outcome.Failure.Code)
		span.SetStatus(codes.Error, outcome.Failure.Code)
	}
	log.Info("attempt ended", attrs...)
}

func (w *Worker) run(ctx context.Context, c *Claim, sink *logSink) domain.Outcome {
	fail := func(stage domain.Stage, code string, err error, art *domain.Artifact) domain.Outcome {
		sink.add(stage, "builder", "error: "+err.Error())
		return domain.Outcome{Status: domain.StatusFailed, Artifact: art, Failure: &domain.Failure{Stage: stage, Code: code, Message: err.Error()}}
	}
	if ctx.Err() != nil {
		return fail(domain.StagePlan, domain.CodeCancelled, context.Cause(ctx), nil)
	}
	var r spec.Resolved
	if err := json.Unmarshal(c.Build.ResolvedSpec, &r); err != nil {
		return fail(domain.StagePlan, domain.CodePlanFailed, err, nil)
	}

	dir := filepath.Join(w.Config.WorkDir, c.Attempt.ID)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(domain.StagePlan, domain.CodePlanFailed, err, nil)
	}
	defer os.RemoveAll(dir)
	sink.add(domain.StagePlan, "builder", fmt.Sprintf("attempt %d of build %s, spec %s", c.Attempt.Number, c.Build.ID, c.Build.SpecDigest))
	p, err := w.Planner.Plan(ctx, r, dir)
	if err != nil {
		return fail(domain.StagePlan, domain.CodePlanFailed, err, nil)
	}
	secrets := map[string][]byte{}
	for _, id := range p.Secrets {
		v, err := w.Secrets.Get(id)
		if err != nil {
			return fail(domain.StagePlan, domain.CodePlanFailed, fmt.Errorf("build secret %s: %w", id, err), nil)
		}
		secrets[id] = v
	}

	tag := DaemonRef(c.Build.ID, c.Attempt.ID)
	sink.add(domain.StageBuild, "builder", "building "+tag)
	img, err := w.Executor.Build(ctx, BuildRequest{
		BuildID: c.Build.ID, AttemptID: c.Attempt.ID, SpecDigest: c.Build.SpecDigest,
		Context: p.Dir, Platform: r.Platform, Tag: tag, Secrets: secrets, NoCache: c.Build.NoCache,
	}, func(stream, text string) { sink.add(domain.StageBuild, stream, text) })
	if err != nil {
		return fail(domain.StageBuild, domain.CodeBuildFailed, err, nil)
	}
	art := &domain.Artifact{ImageID: img.ID, DaemonRef: img.Ref, Platform: r.Platform}
	sink.add(domain.StageBuild, "builder", "built "+img.ID)

	if r.Output.Type == spec.OutputRegistry {
		if w.Publisher == nil {
			return fail(domain.StagePublish, domain.CodePublishFailed, errors.New("registry output is not configured"), art)
		}
		pub, err := w.Publisher.Publish(ctx, img, r.Output.Repository, c.Attempt.ID,
			func(stream, text string) { sink.add(domain.StagePublish, stream, text) })
		if err != nil {
			return fail(domain.StagePublish, domain.CodePublishFailed, err, art)
		}
		art.Repository, art.Digest, art.RegistryRef = pub.Repository, pub.Digest, pub.Ref
		sink.add(domain.StagePublish, "builder", "published "+pub.Ref)
	}
	return domain.Outcome{Status: domain.StatusSucceeded, Artifact: art}
}

// redactor replaces configured secret values in log text.
func redactor(s Secrets) func(string) string {
	if s == nil {
		return func(t string) string { return t }
	}
	var values []string
	for _, v := range s.Values() {
		v = bytes.TrimSpace(v)
		if len(v) >= 4 {
			values = append(values, string(v))
		}
	}
	return func(t string) string {
		for _, v := range values {
			t = strings.ReplaceAll(t, v, "[redacted]")
		}
		return t
	}
}
