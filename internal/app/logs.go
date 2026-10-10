package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

const maxLineBytes = 16 << 10

// logSink writes an attempt's output into the store under the attempt's
// lease, one batch per output chunk; a partial last line waits for the rest
// of its stream. The store marks attempts whose log hit its size limit as
// truncated.
type logSink struct {
	store    Store
	lease    domain.Lease
	maxBytes int64
	redact   func(string) string
	onErr    func(error)

	mu      sync.Mutex
	partial map[string]string
}

func newLogSink(store Store, lease domain.Lease, maxBytes int64, redact func(string) string, onErr func(error)) *logSink {
	return &logSink{store: store, lease: lease, maxBytes: maxBytes, redact: redact, onErr: onErr, partial: map[string]string{}}
}

// add takes output text, which may hold several lines or end mid-line. The
// builder's own messages are whole lines.
func (s *logSink) add(stage domain.Stage, stream, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []string
	if stream == "builder" {
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	} else {
		lines = strings.Split(s.partial[stream]+text, "\n")
		s.partial[stream] = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}
	s.write(stage, stream, lines)
}

func (s *logSink) write(stage domain.Stage, stream string, lines []string) {
	if len(lines) == 0 {
		return
	}
	now := time.Now().UTC()
	entries := make([]domain.LogEntry, len(lines))
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if len(l) > maxLineBytes {
			l = l[:maxLineBytes] + " [line truncated]"
		}
		entries[i] = domain.LogEntry{Time: now, Stage: stage, Stream: stream, Text: s.redact(l)}
	}
	if err := s.store.AppendLogs(context.Background(), s.lease, entries, s.maxBytes); err != nil {
		s.onErr(err)
	}
}

// close writes what is left of partial lines.
func (s *logSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for stream, rest := range s.partial {
		if rest != "" {
			s.write(domain.StageBuild, stream, []string{rest})
		}
	}
	s.partial = map[string]string{}
}
