package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

const (
	logFlushEvery = 500 * time.Millisecond
	logBatch      = 200
	maxLineBytes  = 16 << 10
)

// logSink batches an attempt's log lines into the store under the
// attempt's lease. Lines still buffered when the process dies are lost;
// the store marks attempts whose log hit its size limit as truncated.
type logSink struct {
	store    Store
	lease    domain.Lease
	maxBytes int64
	redact   func(string) string
	clock    Clock
	onErr    func(error)

	mu      sync.Mutex
	buf     []domain.LogEntry
	partial map[string]string
	flushMu sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newLogSink(store Store, lease domain.Lease, maxBytes int64, redact func(string) string, clock Clock, onErr func(error)) *logSink {
	s := &logSink{
		store: store, lease: lease, maxBytes: maxBytes, redact: redact, clock: clock, onErr: onErr,
		partial: map[string]string{}, stop: make(chan struct{}), done: make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		t := time.NewTicker(logFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.flush()
			}
		}
	}()
	return s
}

// add takes output text, which may hold several lines or end mid-line.
func (s *logSink) add(stage domain.Stage, stream, text string) {
	s.mu.Lock()
	var lines []string
	if stream == "builder" {
		// The builder's own messages are whole lines.
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	} else {
		lines = strings.Split(s.partial[stream]+text, "\n")
		s.partial[stream] = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}
	now := s.clock.Now().UTC()
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if len(l) > maxLineBytes {
			l = l[:maxLineBytes] + " [line truncated]"
		}
		s.buf = append(s.buf, domain.LogEntry{Time: now, Stage: stage, Stream: stream, Text: s.redact(l)})
	}
	full := len(s.buf) >= logBatch
	s.mu.Unlock()
	if full {
		go s.flush()
	}
}

func (s *logSink) flush() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := s.store.AppendLogs(context.Background(), s.lease, batch, s.maxBytes); err != nil {
		s.onErr(err)
	}
}

func (s *logSink) close() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		now := s.clock.Now().UTC()
		for stream, rest := range s.partial {
			if rest != "" {
				s.buf = append(s.buf, domain.LogEntry{Time: now, Stage: domain.StageBuild, Stream: stream, Text: s.redact(rest)})
			}
		}
		s.partial = map[string]string{}
		s.mu.Unlock()
		s.flush()
	})
}
