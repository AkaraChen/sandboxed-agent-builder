// Package api is the HTTP adapter: chi routes, authentication, request
// validation, error mapping and pagination. See docs/api.md.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

const (
	maxBody         = 1 << 20
	maxKey          = 200
	defaultAttempts = 20
	maxAttempts     = 100
	defaultLogs     = 500
	maxLogs         = 2000
	// PollInterval is the polling interval the API suggests (Retry-After
	// on non-terminal builds).
	PollInterval = 2
)

// Caller is an API client and the sha256 of its bearer token.
type Caller struct {
	ID          string
	TokenSHA256 string
}

type Server struct {
	Service *app.Service
	Callers []Caller
	// Anonymous serves unauthenticated requests as this caller; only for
	// local development.
	Anonymous string
	Ready     func(context.Context) error
	Log       *slog.Logger
}

type ctxKey struct{}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, s.logRequests)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.ready)
	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		r.Post("/builds", s.submit)
		r.Get("/builds/{id}", s.get)
		r.Get("/builds/{id}/attempts", s.attempts)
		r.Get("/builds/{id}/logs", s.logs)
		r.Post("/builds/{id}/cancel", s.cancel)
		r.Post("/builds/{id}/retry", s.retry)
	})
	return r
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		if r.URL.Path == "/healthz" {
			return
		}
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(), "request_id", middleware.GetReqID(r.Context()))
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := ""
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && token != "" {
			sum := sha256.Sum256([]byte(token))
			for _, c := range s.Callers {
				want, err := hex.DecodeString(c.TokenSHA256)
				if err == nil && subtle.ConstantTimeCompare(sum[:], want) == 1 {
					caller = c.ID
				}
			}
		} else if s.Anonymous != "" {
			caller = s.Anonymous
		}
		if caller == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required", nil)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller)))
	})
}

func callerOf(r *http.Request) string { return r.Context().Value(ctxKey{}).(string) }

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.Ready != nil {
		if err := s.Ready(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", err.Error(), nil)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > maxKey {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "an Idempotency-Key header (1-200 characters) is required", nil)
		return "", false
	}
	return key, true
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "the request body is limited to 1 MiB", nil)
		return
	}
	res, err := s.Service.Submit(r.Context(), callerOf(r), key, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusAccepted
	if res.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Location", "/builds/"+res.Build.ID)
	writeJSON(w, status, buildView(res.Build, res.Attempt))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	v, err := s.Service.Get(r.Context(), callerOf(r), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !v.Current.Status.Terminal() {
		w.Header().Set("Retry-After", strconv.Itoa(PollInterval))
	}
	writeJSON(w, http.StatusOK, buildView(v.Build, v.Current))
}

func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, &domain.SpecError{Problems: []string{name + ": must be an integer from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi)}}
	}
	return n, nil
}

func (s *Server) attempts(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", defaultAttempts, 1, maxAttempts)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, next, err := s.Service.Attempts(r.Context(), callerOf(r), chi.URLParam(r, "id"), app.Page{Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]attemptJSON, len(list))
	for i, a := range list {
		items[i] = attemptView(a)
	}
	out := map[string]any{"items": items, "nextCursor": nil}
	if next != "" {
		out["nextCursor"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", defaultLogs, 1, maxLogs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			s.fail(w, r, &domain.SpecError{Problems: []string{"after: must be a sequence number"}})
			return
		}
	}
	a, entries, err := s.Service.Logs(r.Context(), callerOf(r), chi.URLParam(r, "id"), r.URL.Query().Get("attempt"), after, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	next := after
	if len(entries) > 0 {
		next = entries[len(entries)-1].Seq
	}
	// complete: the attempt ended and this page reached the end of its log.
	complete := a.Status.Terminal() && len(entries) < limit
	writeJSON(w, http.StatusOK, map[string]any{
		"attemptId": a.ID, "status": a.Status, "entries": entries, "nextAfter": next,
		"complete": complete, "truncated": a.LogsTruncated,
	})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	a, err := s.Service.Cancel(r.Context(), callerOf(r), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if a.Status == domain.StatusRunning {
		// Requested, not yet stopped.
		status = http.StatusAccepted
	}
	writeJSON(w, status, attemptView(a))
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	a, created, err := s.Service.Retry(r.Context(), callerOf(r), chi.URLParam(r, "id"), key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, status, attemptView(a))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var se *domain.SpecError
	switch {
	case errors.As(err, &se):
		writeError(w, http.StatusBadRequest, "invalid_request", "the request is invalid", se.Problems)
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such build or attempt", nil)
	case errors.Is(err, domain.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), nil)
	case errors.Is(err, domain.ErrActiveAttempt):
		writeError(w, http.StatusConflict, "active_attempt", "the build's current attempt is still queued or running", nil)
	case errors.Is(err, app.ErrResolve):
		s.Log.WarnContext(r.Context(), "resolve failed", "err", err)
		writeError(w, http.StatusBadGateway, "resolve_failed", err.Error(), nil)
	default:
		s.Log.ErrorContext(r.Context(), "request failed", "err", err, "request_id", middleware.GetReqID(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

type attemptJSON struct {
	AttemptID         string           `json:"attemptId"`
	Number            int              `json:"number"`
	Status            domain.Status    `json:"status"`
	CreatedAt         time.Time        `json:"createdAt"`
	StartedAt         *time.Time       `json:"startedAt"`
	EndedAt           *time.Time       `json:"endedAt"`
	CancelRequestedAt *time.Time       `json:"cancelRequestedAt"`
	Failure           *domain.Failure  `json:"failure"`
	Artifact          *domain.Artifact `json:"artifact"`
	LogsTruncated     bool             `json:"logsTruncated"`
	Recovered         bool             `json:"recovered"`
}

func attemptView(a domain.Attempt) attemptJSON {
	return attemptJSON{
		AttemptID: a.ID, Number: a.Number, Status: a.Status, CreatedAt: a.CreatedAt, StartedAt: a.StartedAt,
		EndedAt: a.EndedAt, CancelRequestedAt: a.CancelRequestedAt, Failure: a.Failure, Artifact: a.Artifact,
		LogsTruncated: a.LogsTruncated, Recovered: a.Recovered,
	}
}

func buildView(b domain.Build, a domain.Attempt) map[string]any {
	var raw struct {
		Spec json.RawMessage `json:"spec"`
	}
	_ = json.Unmarshal(b.RawSpec, &raw)
	return map[string]any{
		"buildId":        b.ID,
		"createdAt":      b.CreatedAt,
		"status":         a.Status,
		"specDigest":     b.SpecDigest,
		"noCache":        b.NoCache,
		"attempts":       b.Attempts,
		"currentAttempt": attemptView(a),
		"spec":           raw.Spec,
		"resolvedSpec":   json.RawMessage(b.ResolvedSpec),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, details []string) {
	body := map[string]any{"code": code, "message": message}
	if len(details) > 0 {
		body["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": body})
}
