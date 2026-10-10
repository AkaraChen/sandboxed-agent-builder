package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/postgres"
	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/postgres/pgtest"
	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

type resolver struct{}

func (resolver) Resolve(_ context.Context, s spec.Spec) (spec.Resolved, error) {
	return spec.Resolved{Contract: 1, Platform: s.Platform, Scripts: s.Scripts, Output: s.Output}, nil
}

func hash(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

func server(t *testing.T) (*httptest.Server, *postgres.Store) {
	store, err := postgres.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	policy := spec.Policy{Platforms: []string{"linux/amd64"}, PluginSource: func(string) bool { return false }}
	svc := &app.Service{Store: store, Resolver: resolver{}, Policy: func() spec.Policy { return policy }, Log: log}
	s := &Server{Service: svc, Log: log, Callers: []Caller{{ID: "axp", TokenSHA256: hash("axp-token")}, {ID: "other", TokenSHA256: hash("other-token")}}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func do(t *testing.T, ts *httptest.Server, method, path, token, key, body string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestBuildLifecycle(t *testing.T) {
	ts, store := server(t)
	body := `{"spec":{"scripts":{"startup":[{"name":"s","run":"true"}]}}}`
	if code, _, _ := do(t, ts, "POST", "/builds", "", "k", body); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _, m := do(t, ts, "POST", "/builds", "axp-token", "", body); code != 400 || errCode(m) != "idempotency_key_required" {
		t.Fatalf("no key: %d %v", code, m)
	}
	code, h, m := do(t, ts, "POST", "/builds", "axp-token", "k", body)
	if code != 202 || m["status"] != "queued" || h.Get("Location") == "" {
		t.Fatalf("submit: %d %v", code, m)
	}
	id := m["buildId"].(string)
	code, h, m = do(t, ts, "POST", "/builds", "axp-token", "k", body)
	if code != 200 || h.Get("Idempotent-Replayed") != "true" || m["buildId"] != id {
		t.Fatalf("replay: %d %v", code, m)
	}
	if code, _, m := do(t, ts, "POST", "/builds", "axp-token", "k", `{"spec":{}}`); code != 409 || errCode(m) != "idempotency_conflict" {
		t.Fatalf("conflict: %d %v", code, m)
	}
	if code, _, m := do(t, ts, "POST", "/builds", "axp-token", "k2", `{"spec":{"base":{"family":"ubuntu"}}}`); code != 400 || errCode(m) != "invalid_request" {
		t.Fatalf("invalid: %d %v", code, m)
	}
	code, h, m = do(t, ts, "GET", "/builds/"+id, "axp-token", "", "")
	if code != 200 || h.Get("Retry-After") == "" || m["spec"] == nil || m["resolvedSpec"] == nil {
		t.Fatalf("get: %d %v", code, m)
	}
	if code, _, _ := do(t, ts, "GET", "/builds/"+id, "other-token", "", ""); code != 404 {
		t.Fatalf("other caller: %d", code)
	}
	if code, _, m := do(t, ts, "POST", "/builds/"+id+"/retry", "axp-token", "r1", ""); code != 409 || errCode(m) != "active_attempt" {
		t.Fatalf("retry while queued: %d %v", code, m)
	}

	// A worker fails the attempt; retry makes attempt 2.
	c, _ := store.Claim(context.Background(), "w", time.Now(), time.Now().Add(time.Minute))
	_ = store.AppendLogs(context.Background(), c.Lease, nil, 0)
	failWith(t, store, c)
	code, _, m = do(t, ts, "POST", "/builds/"+id+"/retry", "axp-token", "r1", "")
	if code != 202 || m["number"].(float64) != 2 {
		t.Fatalf("retry: %d %v", code, m)
	}
	if code, h, _ := do(t, ts, "POST", "/builds/"+id+"/retry", "axp-token", "r1", ""); code != 200 || h.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry replay: %d", code)
	}
	code, _, m = do(t, ts, "GET", "/builds/"+id+"/attempts?limit=1", "axp-token", "", "")
	if code != 200 || len(m["items"].([]any)) != 1 || m["nextCursor"] != "1" {
		t.Fatalf("attempts: %d %v", code, m)
	}
	code, _, m = do(t, ts, "GET", "/builds/"+id+"/attempts?limit=1&cursor=1", "axp-token", "", "")
	if code != 200 || m["nextCursor"] != nil || m["items"].([]any)[0].(map[string]any)["number"].(float64) != 2 {
		t.Fatalf("attempts page 2: %d %v", code, m)
	}
	if code, _, _ := do(t, ts, "GET", "/builds/"+id+"/attempts?limit=1000", "axp-token", "", ""); code != 400 {
		t.Fatalf("limit: %d", code)
	}
	first := c.Attempt.ID
	code, _, m = do(t, ts, "GET", "/builds/"+id+"/logs?attempt="+first+"&after=0&limit=1", "axp-token", "", "")
	if code != 200 || len(m["entries"].([]any)) != 1 || m["nextAfter"].(float64) != 1 || m["complete"] != false {
		t.Fatalf("logs: %d %v", code, m)
	}
	code, _, m = do(t, ts, "GET", "/builds/"+id+"/logs?attempt="+first+"&after=1", "axp-token", "", "")
	if code != 200 || len(m["entries"].([]any)) != 0 || m["complete"] != true {
		t.Fatalf("logs end: %d %v", code, m)
	}
	code, _, m = do(t, ts, "POST", "/builds/"+id+"/cancel", "axp-token", "", "")
	if code != 200 || m["status"] != "cancelled" {
		t.Fatalf("cancel queued: %d %v", code, m)
	}
}

func failWith(t *testing.T, store *postgres.Store, c *app.Claim) {
	t.Helper()
	ctx := context.Background()
	if err := store.AppendLogs(ctx, c.Lease, logLines("failed step"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, c.Lease, failed(), time.Now(), false); err != nil {
		t.Fatal(err)
	}
}
