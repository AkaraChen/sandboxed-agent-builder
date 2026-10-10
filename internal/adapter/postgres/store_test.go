package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/postgres/pgtest"
	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := pgtest.URL(t)
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

var t0 = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

func newBuild(id, caller, key string, at time.Time) app.NewBuild {
	return app.NewBuild{
		Build: domain.Build{ID: id, Caller: caller, IdempotencyKey: key, RequestHash: "h-" + key, SpecDigest: "sha256:spec",
			RawSpec: []byte(`{"spec":{}}`), ResolvedSpec: []byte(`{}`), CreatedAt: at},
		Attempt: domain.Attempt{ID: id + "-a1", BuildID: id, Number: 1, Status: domain.StatusQueued, CreatedAt: at},
	}
}

func mustCreate(t *testing.T, s *Store, nb app.NewBuild) {
	t.Helper()
	if _, created, err := s.CreateBuild(context.Background(), nb); err != nil || !created {
		t.Fatalf("create %s: %v %v", nb.Build.ID, created, err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	s, path := open(t)
	mustCreate(t, s, newBuild("b1", "c", "k", t0))
	s.Close()
	// Reopening applies nothing new; two instances migrating at once wait
	// on goose's session lock.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Error(err)
				return
			}
			s.Close()
		})
	}
	wg.Wait()
	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetBuild(context.Background(), "c", "b1"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBuildIdempotency(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b1", "c", "k", t0))
	got, created, err := s.CreateBuild(ctx, newBuild("b2", "c", "k", t0))
	if err != nil || created || got.ID != "b1" {
		t.Fatalf("replay: %v %v %v", got.ID, created, err)
	}
	// Same key, other caller: a separate build.
	mustCreate(t, s, newBuild("b3", "other", "k", t0))
	if _, err := s.GetBuild(ctx, "other", "b1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("caller isolation: %v", err)
	}
}

func TestConcurrentClaimTakesEachAttemptOnce(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	const builds = 40
	for i := range builds {
		mustCreate(t, s, newBuild(fmt.Sprintf("b%02d", i), "c", fmt.Sprintf("k%d", i), t0.Add(time.Duration(i)*time.Millisecond)))
	}
	// Two pools stand for two builder instances.
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for w := range 8 {
		store := s
		if w%2 == 1 {
			store = other
		}
		wg.Go(func() {
			worker := fmt.Sprintf("w%d", w)
			for {
				c, err := store.Claim(ctx, worker, t0, t0.Add(time.Minute))
				if err != nil {
					t.Error(err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				if prev, dup := seen[c.Attempt.ID]; dup {
					t.Errorf("%s claimed by %s and %s", c.Attempt.ID, prev, worker)
				}
				seen[c.Attempt.ID] = worker
				mu.Unlock()
				if c.Attempt.Status != domain.StatusRunning || c.Lease.Generation != 1 {
					t.Errorf("claim state %+v", c.Attempt)
				}
			}
		})
	}
	wg.Wait()
	if len(seen) != builds {
		t.Fatalf("claimed %d of %d", len(seen), builds)
	}
}

func TestClaimOrderIsFIFO(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("late", "c", "k2", t0.Add(time.Second)))
	mustCreate(t, s, newBuild("early", "c", "k1", t0))
	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	if c.Build.ID != "early" {
		t.Fatalf("claimed %s", c.Build.ID)
	}
}

func TestStaleLeaseIsFenced(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b1", "c", "k", t0))
	old, _ := s.Claim(ctx, "w1", t0, t0.Add(10*time.Second))
	if _, err := s.Renew(ctx, old.Lease, t0.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if early, _ := s.TakeOver(ctx, "reconciler", t0.Add(15*time.Second), t0.Add(time.Minute)); early != nil {
		t.Fatal("took over a renewed lease")
	}
	taken, err := s.TakeOver(ctx, "reconciler", t0.Add(25*time.Second), t0.Add(time.Minute))
	if err != nil || taken == nil || taken.Lease.Generation != 2 {
		t.Fatalf("take over: %+v %v", taken, err)
	}
	// A second takeover of a live lease does nothing.
	if again, _ := s.TakeOver(ctx, "x", t0.Add(26*time.Second), t0.Add(time.Minute)); again != nil {
		t.Fatal("took over a live lease")
	}
	ok := domain.Outcome{Status: domain.StatusSucceeded, Artifact: &domain.Artifact{ImageID: "sha256:x"}}
	if _, err := s.Renew(ctx, old.Lease, t0.Add(time.Hour)); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("stale renew: %v", err)
	}
	if err := s.AppendLogs(ctx, old.Lease, []domain.LogEntry{{Text: "late"}}, 0); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("stale logs: %v", err)
	}
	if err := s.Finish(ctx, old.Lease, ok, t0, false); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("stale finish: %v", err)
	}
	fail := domain.Outcome{Status: domain.StatusInterrupted, Failure: &domain.Failure{Stage: domain.StageReconcile, Code: domain.CodeInterrupted}}
	if err := s.Finish(ctx, taken.Lease, fail, t0, true); err != nil {
		t.Fatal(err)
	}
	a, _ := s.GetAttempt(ctx, "b1", "b1-a1")
	if a.Status != domain.StatusInterrupted || !a.Recovered || a.LeaseExpiresAt != nil {
		t.Fatalf("attempt %+v", a)
	}
	// Terminal: nothing more is written.
	if err := s.Finish(ctx, taken.Lease, ok, t0, true); !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("finish twice: %v", err)
	}
}

func TestFinishValidatesOutcome(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b1", "c", "k", t0))
	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	if err := s.Finish(ctx, c.Lease, domain.Outcome{Status: domain.StatusSucceeded}, t0, false); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("success without artifact: %v", err)
	}
	if err := s.Finish(ctx, c.Lease, domain.Outcome{Status: domain.StatusQueued, Failure: &domain.Failure{}}, t0, false); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("back to queued: %v", err)
	}
}

func TestCancel(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("q", "c", "k1", t0))
	a, err := s.RequestCancel(ctx, "q", t0)
	if err != nil || a.Status != domain.StatusCancelled || a.CancelRequestedAt == nil {
		t.Fatalf("queued cancel: %+v %v", a, err)
	}
	if c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute)); c != nil {
		t.Fatal("claimed a cancelled attempt")
	}

	mustCreate(t, s, newBuild("r", "c", "k2", t0))
	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	a, _ = s.RequestCancel(ctx, "r", t0.Add(time.Second))
	if a.Status != domain.StatusRunning || a.CancelRequestedAt == nil {
		t.Fatalf("running cancel must stay running until stopped: %+v", a)
	}
	if cancel, _ := s.Renew(ctx, c.Lease, t0.Add(time.Minute)); !cancel {
		t.Fatal("renew does not report the cancel request")
	}
	// The build finished before the worker saw the request: success stands.
	if err := s.Finish(ctx, c.Lease, domain.Outcome{Status: domain.StatusSucceeded, Artifact: &domain.Artifact{ImageID: "sha256:x"}}, t0, false); err != nil {
		t.Fatal(err)
	}
	a, _ = s.RequestCancel(ctx, "r", t0.Add(2*time.Second))
	if a.Status != domain.StatusSucceeded {
		t.Fatalf("late cancel overwrote success: %+v", a)
	}
}

func TestRetry(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b", "c", "k", t0))
	next := domain.Attempt{ID: "b-a2", BuildID: "b", Status: domain.StatusQueued, CreatedAt: t0}
	if _, _, err := s.CreateRetry(ctx, "c", "r1", "h", next); !errors.Is(err, domain.ErrActiveAttempt) {
		t.Fatalf("retry while queued: %v", err)
	}
	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	_ = s.AppendLogs(ctx, c.Lease, []domain.LogEntry{{Time: t0, Stage: domain.StageBuild, Stream: "buildkit", Text: "boom"}}, 0)
	_ = s.Finish(ctx, c.Lease, domain.Outcome{Status: domain.StatusFailed, Failure: &domain.Failure{Stage: domain.StageBuild, Code: domain.CodeBuildFailed, Message: "boom"}}, t0, false)

	a, created, err := s.CreateRetry(ctx, "c", "r1", "h", next)
	if err != nil || !created || a.Number != 2 || a.Status != domain.StatusQueued {
		t.Fatalf("retry: %+v %v %v", a, created, err)
	}
	again, created, err := s.CreateRetry(ctx, "c", "r1", "h", domain.Attempt{ID: "b-a3", BuildID: "b", CreatedAt: t0})
	if err != nil || created || again.ID != "b-a2" {
		t.Fatalf("replayed retry: %+v %v %v", again, created, err)
	}
	if _, _, err := s.CreateRetry(ctx, "c", "r1", "other", next); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("key reuse: %v", err)
	}
	b, _ := s.GetBuild(ctx, "c", "b")
	if b.CurrentAttemptID != "b-a2" || b.Attempts != 2 {
		t.Fatalf("build %+v", b)
	}
	old, _ := s.GetAttempt(ctx, "b", "b-a1")
	logs, _ := s.ListLogs(ctx, "b-a1", 0, 10)
	if old.Status != domain.StatusFailed || old.Failure.Message != "boom" || len(logs) != 1 {
		t.Fatalf("first attempt not kept: %+v %v", old, logs)
	}
}

func TestPagination(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b", "c", "k", t0))
	for i := 2; i <= 5; i++ {
		c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
		_ = s.Finish(ctx, c.Lease, domain.Outcome{Status: domain.StatusFailed, Failure: &domain.Failure{Stage: domain.StageBuild, Code: "x"}}, t0, false)
		if _, _, err := s.CreateRetry(ctx, "c", fmt.Sprint(i), "h", domain.Attempt{ID: fmt.Sprintf("b-a%d", i), BuildID: "b", CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	var numbers []int
	cursor := ""
	for pages := 0; ; pages++ {
		list, next, err := s.ListAttempts(ctx, "b", app.Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			numbers = append(numbers, a.Number)
		}
		if next == "" {
			if pages != 2 {
				t.Fatalf("pages %d", pages+1)
			}
			break
		}
		cursor = next
	}
	if fmt.Sprint(numbers) != "[1 2 3 4 5]" {
		t.Fatalf("numbers %v", numbers)
	}
	if _, _, err := s.ListAttempts(ctx, "b", app.Page{Limit: 2, Cursor: "x"}); err == nil {
		t.Fatal("bad cursor accepted")
	}

	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	var entries []domain.LogEntry
	for i := range 25 {
		entries = append(entries, domain.LogEntry{Time: t0, Stage: domain.StageBuild, Stream: "buildkit", Text: fmt.Sprint("line ", i)})
	}
	if err := s.AppendLogs(ctx, c.Lease, entries[:10], 0); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogs(ctx, c.Lease, entries[10:], 0); err != nil {
		t.Fatal(err)
	}
	var got []int64
	after := int64(0)
	for {
		page, err := s.ListLogs(ctx, c.Attempt.ID, after, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			got = append(got, e.Seq)
		}
		after = page[len(page)-1].Seq
	}
	if len(got) != 25 || got[0] != 1 || got[24] != 25 {
		t.Fatalf("seqs %v", got)
	}
}

func TestLogLimitTruncates(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b", "c", "k", t0))
	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	big := []domain.LogEntry{{Text: "0123456789"}, {Text: "0123456789"}, {Text: "0123456789"}}
	if err := s.AppendLogs(ctx, c.Lease, big, 25); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogs(ctx, c.Lease, big, 25); err != nil {
		t.Fatal(err)
	}
	logs, _ := s.ListLogs(ctx, c.Attempt.ID, 0, 100)
	if len(logs) != 3 || logs[2].Text != truncatedNote {
		t.Fatalf("logs %+v", logs)
	}
	a, _ := s.GetAttempt(ctx, "b", c.Attempt.ID)
	if !a.LogsTruncated {
		t.Fatal("not marked truncated")
	}
}

func TestLogTextIsStorable(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	mustCreate(t, s, newBuild("b", "c", "k", t0))
	c, _ := s.Claim(ctx, "w", t0, t0.Add(time.Minute))
	if err := s.AppendLogs(ctx, c.Lease, []domain.LogEntry{{Time: t0, Stage: domain.StageBuild, Stream: "buildkit", Text: "nul\x00byte bad\xffutf8"}}, 0); err != nil {
		t.Fatal(err)
	}
	logs, _ := s.ListLogs(ctx, c.Attempt.ID, 0, 10)
	if len(logs) != 1 || logs[0].Text != "nulbyte bad�utf8" || !logs[0].Time.Equal(t0) {
		t.Fatalf("%+v", logs)
	}
	b, _ := s.GetBuild(ctx, "c", "b")
	if b.CreatedAt.Location() != time.UTC || !b.CreatedAt.Equal(t0) {
		t.Fatalf("created_at %s", b.CreatedAt)
	}
}
