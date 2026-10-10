package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/sqlite"
	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

type resolver struct{ calls atomic.Int64 }

func (r *resolver) Resolve(_ context.Context, s spec.Spec) (spec.Resolved, error) {
	r.calls.Add(1)
	return spec.Resolved{
		Contract: 1, Platform: s.Platform, Scripts: s.Scripts, Output: s.Output,
		Base:  spec.ResolvedImage{Family: s.Base.Family, Version: s.Base.Version, Image: "debian", Digest: "sha256:base"},
		Agent: spec.ResolvedAgent{Type: s.Agent.Type, Codex: &spec.ResolvedPackage{Package: "@openai/codex", Version: s.Agent.Codex.Version}},
		Oar:   spec.ResolvedPackage{Package: "@botiverse/oar-cli", Version: "0.51.0"},
	}, nil
}

// executor builds by calling fn, or blocks until ctx is done when block is
// set.
type executor struct {
	mu     sync.Mutex
	images map[string]*app.Image
	fn     func(ctx context.Context, req app.BuildRequest, log app.LogFunc) (app.Image, error)
}

func (e *executor) Build(ctx context.Context, req app.BuildRequest, log app.LogFunc) (app.Image, error) {
	img, err := e.fn(ctx, req, log)
	if err == nil {
		e.mu.Lock()
		e.images[req.AttemptID] = &img
		e.mu.Unlock()
	}
	return img, err
}

func (e *executor) FindAttemptImage(_ context.Context, _, attemptID string) (*app.Image, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.images[attemptID], nil
}

type publisher struct {
	err   error
	found map[string]*app.Published
}

func (p *publisher) Publish(_ context.Context, img app.Image, repo, tag string, _ app.LogFunc) (app.Published, error) {
	if p.err != nil {
		return app.Published{}, p.err
	}
	return app.Published{Repository: "reg/" + repo, Digest: "sha256:pushed", Ref: "reg/" + repo + "@sha256:pushed"}, nil
}

func (p *publisher) Find(_ context.Context, repo, tag string) (*app.Published, error) {
	return p.found[repo+":"+tag], nil
}

type env struct {
	store    *sqlite.Store
	service  *app.Service
	worker   *app.Worker
	exec     *executor
	pub      *publisher
	resolver *resolver
}

func setup(t *testing.T) *env {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &env{store: store, resolver: &resolver{}, pub: &publisher{found: map[string]*app.Published{}}}
	e.exec = &executor{images: map[string]*app.Image{}, fn: func(_ context.Context, req app.BuildRequest, log app.LogFunc) (app.Image, error) {
		log("buildkit", "step 1\nstep 2\n")
		return app.Image{ID: "sha256:img-" + req.AttemptID, Ref: req.Tag, Platform: req.Platform}, nil
	}}
	policy := spec.Policy{Platforms: []string{"linux/amd64"}, Secrets: []string{"tok"}, Registry: true, PluginSource: func(string) bool { return true }}
	e.service = &app.Service{Store: store, Resolver: e.resolver, Policy: func() spec.Policy { return policy }, Log: log}
	e.worker = &app.Worker{
		Store: store, Executor: e.exec, Publisher: e.pub, Secrets: map[string][]byte{"tok": []byte("very-secret-token")}, Log: log,
		Config: app.WorkerConfig{ID: "w1", Concurrency: 1, Poll: 10 * time.Millisecond, Lease: 3 * time.Second, WorkDir: t.TempDir(), MaxLogBytes: 1 << 20, Grace: time.Second},
	}
	return e
}

const body = `{"spec":{"scripts":{"build":[{"name":"one","run":"echo one","secrets":["tok"]}]}}}`

func (e *env) submit(t *testing.T, key, body string) app.Submitted {
	t.Helper()
	res, err := e.service.Submit(context.Background(), "axp", key, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) runOne(t *testing.T) {
	t.Helper()
	ran, err := e.worker.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("run: %v %v", ran, err)
	}
}

func (e *env) current(t *testing.T, id string) domain.Attempt {
	t.Helper()
	v, err := e.service.Get(context.Background(), "axp", id)
	if err != nil {
		t.Fatal(err)
	}
	return v.Current
}

func TestSubmitIdempotency(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first := e.submit(t, "k1", body)
	if first.Replayed || first.Attempt.Status != domain.StatusQueued {
		t.Fatalf("first: %+v", first)
	}
	// Same content, other key order and whitespace: a replay, not resolved again.
	reordered := `{ "spec": { "scripts": { "build": [ { "run": "echo one", "secrets": ["tok"], "name": "one" } ] } } }`
	again := e.submit(t, "k1", reordered)
	if !again.Replayed || again.Build.ID != first.Build.ID || e.resolver.calls.Load() != 1 {
		t.Fatalf("replay: %+v resolves=%d", again, e.resolver.calls.Load())
	}
	if _, err := e.service.Submit(ctx, "axp", "k1", []byte(`{"spec":{}}`)); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// The same spec under a new key is a new build with the same digest.
	rebuild := e.submit(t, "k2", body)
	if rebuild.Replayed || rebuild.Build.ID == first.Build.ID || rebuild.Build.SpecDigest != first.Build.SpecDigest {
		t.Fatalf("rebuild: %+v", rebuild.Build)
	}
	// Another caller does not see the build.
	if _, err := e.service.Get(ctx, "someone-else", first.Build.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("isolation: %v", err)
	}
}

func TestSubmitRejectsInvalidSpecs(t *testing.T) {
	e := setup(t)
	for _, b := range []string{
		`{"spec":{"base":{"family":"ubuntu"}}}`,
		`{"spec":{"base":{"family":"alpine"}}}`,
		`{"spec":{"scripts":{"build":[{"name":"x","run":"y","secrets":["nope"]}]}}}`,
		`{"spec":{"scripts":{"startup":[{"name":"x","run":"y","secrets":["tok"]}]}}}`,
		`{"spec":{"unknown":1}}`,
		`{"spec":{"codex":{"version":"latest"}}}`,
		`{"spec":{"agent":{"type":"claude"}}}`,
		`{"spec":{"agent":{"type":"codex","codex":{"version":"^1"}}}}`,
		`{"spec":{"output":{"type":"registry","repository":"Bad Name"}}}`,
	} {
		_, err := e.service.Submit(context.Background(), "axp", "k", []byte(b))
		var se *domain.SpecError
		if !errors.As(err, &se) {
			t.Errorf("%s: %v", b, err)
		}
	}
	if e.resolver.calls.Load() != 0 {
		t.Fatal("invalid specs were resolved")
	}
}

func TestWorkerSucceedsAndRedactsSecrets(t *testing.T) {
	e := setup(t)
	e.exec.fn = func(_ context.Context, req app.BuildRequest, log app.LogFunc) (app.Image, error) {
		if string(req.Secrets["tok"]) != "very-secret-token" {
			return app.Image{}, errors.New("secret not passed")
		}
		log("buildkit", "token is very-secret-token\npartial line without newline")
		return app.Image{ID: "sha256:img", Ref: req.Tag}, nil
	}
	s := e.submit(t, "k", body)
	e.runOne(t)
	a := e.current(t, s.Build.ID)
	if a.Status != domain.StatusSucceeded || a.Artifact.ImageID != "sha256:img" || a.Artifact.DaemonRef != app.DaemonRef(s.Build.ID, a.ID) {
		t.Fatalf("attempt %+v", a)
	}
	_, logs, _ := e.service.Logs(context.Background(), "axp", s.Build.ID, "", 0, 100)
	var text []string
	for _, l := range logs {
		text = append(text, l.Text)
	}
	all := strings.Join(text, "\n")
	if strings.Contains(all, "very-secret-token") || !strings.Contains(all, "token is [redacted]") || !strings.Contains(all, "partial line without newline") {
		t.Fatalf("logs:\n%s", all)
	}
}

func TestFailureThenRetryKeepsHistory(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	fail := true
	e.exec.fn = func(_ context.Context, req app.BuildRequest, log app.LogFunc) (app.Image, error) {
		if fail {
			log("buildkit", "compiling\n")
			return app.Image{}, errors.New("process exited 1")
		}
		return app.Image{ID: "sha256:ok", Ref: req.Tag}, nil
	}
	s := e.submit(t, "k", body)
	e.runOne(t)
	first := e.current(t, s.Build.ID)
	if first.Status != domain.StatusFailed || first.Failure.Stage != domain.StageBuild || first.Failure.Code != domain.CodeBuildFailed {
		t.Fatalf("first %+v", first)
	}
	fail = false
	retry, created, err := e.service.Retry(ctx, "axp", s.Build.ID, "retry-1")
	if err != nil || !created || retry.Number != 2 {
		t.Fatalf("retry %+v %v %v", retry, created, err)
	}
	if _, _, err := e.service.Retry(ctx, "axp", s.Build.ID, "retry-2"); !errors.Is(err, domain.ErrActiveAttempt) {
		t.Fatalf("second retry while queued: %v", err)
	}
	e.runOne(t)
	if a := e.current(t, s.Build.ID); a.Status != domain.StatusSucceeded || a.Number != 2 {
		t.Fatalf("retry result %+v", a)
	}
	list, _, _ := e.service.Attempts(ctx, "axp", s.Build.ID, app.Page{Limit: 10})
	if len(list) != 2 || list[0].Status != domain.StatusFailed || list[0].Failure.Message != "process exited 1" {
		t.Fatalf("history %+v", list)
	}
	_, logs, _ := e.service.Logs(ctx, "axp", s.Build.ID, first.ID, 0, 100)
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1].Text, "process exited 1") {
		t.Fatalf("first attempt logs %+v", logs)
	}
	if e.resolver.calls.Load() != 1 {
		t.Fatal("retry resolved the spec again")
	}
}

func TestCancelRunningStopsBuild(t *testing.T) {
	e := setup(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	e.exec.fn = func(ctx context.Context, _ app.BuildRequest, _ app.LogFunc) (app.Image, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return app.Image{}, ctx.Err()
	}
	s := e.submit(t, "k", body)
	done := make(chan struct{})
	go func() { e.runOne(t); close(done) }()
	<-started
	a, err := e.service.Cancel(context.Background(), "axp", s.Build.ID)
	if err != nil || a.Status != domain.StatusRunning || a.CancelRequestedAt == nil {
		t.Fatalf("cancel accepted %+v %v", a, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the build did not stop")
	}
	<-stopped
	a = e.current(t, s.Build.ID)
	if a.Status != domain.StatusCancelled || a.Failure.Code != domain.CodeCancelled {
		t.Fatalf("after stop %+v", a)
	}
}

func TestCancelQueued(t *testing.T) {
	e := setup(t)
	s := e.submit(t, "k", body)
	a, _ := e.service.Cancel(context.Background(), "axp", s.Build.ID)
	if a.Status != domain.StatusCancelled {
		t.Fatalf("%+v", a)
	}
	if ran, _ := e.worker.RunOnce(context.Background()); ran {
		t.Fatal("ran a cancelled attempt")
	}
}

func TestLostLeaseWritesNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	release := make(chan struct{})
	e.exec.fn = func(ctx context.Context, req app.BuildRequest, _ app.LogFunc) (app.Image, error) {
		<-release
		return app.Image{ID: "sha256:late", Ref: req.Tag}, nil
	}
	s := e.submit(t, "k", body)
	done := make(chan struct{})
	go func() { e.runOne(t); close(done) }()
	// Another process takes the attempt over after the lease (pretend it
	// expired) and records it interrupted.
	var claim *app.Claim
	for claim == nil {
		time.Sleep(20 * time.Millisecond)
		claim, _ = e.store.TakeOver(ctx, "reconciler", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	}
	if err := e.store.Finish(ctx, claim.Lease, domain.Outcome{Status: domain.StatusInterrupted,
		Failure: &domain.Failure{Stage: domain.StageReconcile, Code: domain.CodeInterrupted}}, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if a := e.current(t, s.Build.ID); a.Status != domain.StatusInterrupted {
		t.Fatalf("stale worker overwrote the outcome: %+v", a)
	}
}

func TestPublishFailureIsRecorded(t *testing.T) {
	e := setup(t)
	e.pub.err = errors.New("connection refused")
	s := e.submit(t, "k", `{"spec":{"output":{"type":"registry","repository":"agents/demo"}}}`)
	e.runOne(t)
	a := e.current(t, s.Build.ID)
	if a.Status != domain.StatusFailed || a.Failure.Stage != domain.StagePublish || a.Artifact == nil || a.Artifact.RegistryRef != "" {
		t.Fatalf("%+v %+v", a, a.Artifact)
	}
	e.pub.err = nil
	if _, _, err := e.service.Retry(context.Background(), "axp", s.Build.ID, "r"); err != nil {
		t.Fatal(err)
	}
	e.runOne(t)
	if a := e.current(t, s.Build.ID); a.Status != domain.StatusSucceeded || a.Artifact.RegistryRef != "reg/agents/demo@sha256:pushed" {
		t.Fatalf("%+v", a.Artifact)
	}
}

func TestReconcile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := e.worker

	// A worker claimed three attempts and died: one finished its image,
	// one did not, one (registry) built but its push is unconfirmed.
	built := e.submit(t, "a", body)
	lost := e.submit(t, "b", body)
	unpublished := e.submit(t, "c", `{"spec":{"output":{"type":"registry","repository":"agents/x"}}}`)
	past := time.Now().Add(-time.Hour)
	for range 3 {
		if c, err := e.store.Claim(ctx, "dead-worker", past, past.Add(time.Second)); err != nil || c == nil {
			t.Fatal(c, err)
		}
	}
	e.exec.images[built.Attempt.ID] = &app.Image{ID: "sha256:built", Ref: app.DaemonRef(built.Build.ID, built.Attempt.ID)}
	e.exec.images[unpublished.Attempt.ID] = &app.Image{ID: "sha256:built2"}

	n, err := r.Reconcile(ctx)
	if err != nil || n != 3 {
		t.Fatalf("reconciled %d %v", n, err)
	}
	if a := e.current(t, built.Build.ID); a.Status != domain.StatusSucceeded || !a.Recovered || a.Artifact.ImageID != "sha256:built" {
		t.Fatalf("built %+v", a)
	}
	if a := e.current(t, lost.Build.ID); a.Status != domain.StatusInterrupted || a.Failure.Code != domain.CodeInterrupted {
		t.Fatalf("lost %+v", a)
	}
	if a := e.current(t, unpublished.Build.ID); a.Status != domain.StatusInterrupted || a.Failure.Stage != domain.StagePublish {
		t.Fatalf("unpublished %+v", a)
	}
	// Live leases are left alone.
	live := e.submit(t, "d", body)
	if c, _ := e.store.Claim(ctx, "alive", time.Now(), time.Now().Add(time.Hour)); c == nil {
		t.Fatal("no claim")
	}
	if n, _ := r.Reconcile(ctx); n != 0 {
		t.Fatalf("reconciled a live attempt")
	}
	if a := e.current(t, live.Build.ID); a.Status != domain.StatusRunning {
		t.Fatalf("live %+v", a)
	}
}

func TestShutdownInterruptsRunningAttempts(t *testing.T) {
	e := setup(t)
	e.worker.Config.Grace = 50 * time.Millisecond
	started := make(chan struct{})
	e.exec.fn = func(ctx context.Context, _ app.BuildRequest, _ app.LogFunc) (app.Image, error) {
		close(started)
		<-ctx.Done()
		return app.Image{}, ctx.Err()
	}
	s := e.submit(t, "k", body)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.worker.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	if a := e.current(t, s.Build.ID); a.Status != domain.StatusInterrupted {
		t.Fatalf("%+v", a)
	}
}
