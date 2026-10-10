// Package gitsource fetches plugin repositories with the git CLI: a
// revision is locked to a commit when a build is submitted, and attempts
// fetch exactly that commit.
package gitsource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrUnknownRevision is a revision the repository does not have.
var ErrUnknownRevision = errors.New("unknown revision")

type Git struct {
	// Dir caches checkouts, one per source and commit.
	Dir string
	// AllowFile permits file:// sources (tests and local development).
	AllowFile bool
	Timeout   time.Duration

	mu sync.Mutex
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (g *Git) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	proto := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}
	if g.AllowFile {
		proto = append(proto, "-c", "protocol.file.allow=always")
	}
	cmd := exec.CommandContext(ctx, "git", append(proto, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Allowed reports whether source is a URL this fetcher may use.
func (g *Git) Allowed(source string) bool {
	if strings.HasPrefix(source, "https://") {
		return true
	}
	return g.AllowFile && strings.HasPrefix(source, "file://")
}

// ResolveRevision locks revision (a commit, tag or branch) to a commit.
func (g *Git) ResolveRevision(ctx context.Context, source, revision string) (string, error) {
	if !g.Allowed(source) {
		return "", fmt.Errorf("source %q is not allowed", source)
	}
	if commitRE.MatchString(revision) {
		return revision, nil
	}
	out, err := g.git(ctx, "", "ls-remote", source, revision, "refs/tags/"+revision+"^{}")
	if err != nil {
		return "", err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			refs[f[1]] = f[0]
		}
	}
	for _, ref := range []string{"refs/tags/" + revision + "^{}", "refs/tags/" + revision, "refs/heads/" + revision, revision} {
		if c, ok := refs[ref]; ok {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w %q in %s", ErrUnknownRevision, revision, source)
}

// Fetch returns a checkout of source at commit. Checkouts are cached and
// must not be changed.
func (g *Git) Fetch(source, commit string) (string, error) {
	if !g.Allowed(source) {
		return "", fmt.Errorf("source %q is not allowed", source)
	}
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("%q is not a commit", commit)
	}
	sum := sha256.Sum256([]byte(source))
	dir := filepath.Join(g.Dir, hex.EncodeToString(sum[:8]), commit)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, ".git", "sab-complete")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".fetch-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ctx := context.Background()
	steps := [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", source, commit},
		{"-c", "advice.detachedHead=false", "checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if _, err := g.git(ctx, tmp, s...); err != nil {
			if s[0] == "fetch" {
				return "", fmt.Errorf("%w %s in %s: %v", ErrUnknownRevision, commit, source, err)
			}
			return "", err
		}
	}
	head, err := g.git(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if got := strings.TrimSpace(string(head)); got != commit {
		return "", fmt.Errorf("fetched %s, wanted %s", got, commit)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".git", "sab-complete"), nil, 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}
