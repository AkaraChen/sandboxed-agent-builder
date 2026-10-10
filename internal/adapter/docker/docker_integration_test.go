//go:build docker

package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/app"
)

// Run with: go test -tags docker ./internal/adapter/docker
func TestBuildWithSecretAndCancel(t *testing.T) {
	d, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	platform, err := d.Platform(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM busybox:1.37
RUN --mount=type=secret,id=tok,required=true test "$(sha256sum /run/secrets/tok | cut -c1-16)" = 1f3fa74b1208842a && echo secret-ok
RUN echo built > /built
`), 0o644)
	var mu sync.Mutex
	var out strings.Builder
	log := func(_, text string) { mu.Lock(); out.WriteString(text); mu.Unlock() }
	req := app.BuildRequest{BuildID: "bld_test", AttemptID: "att_test1", SpecDigest: "sha256:x", Context: dir, Platform: platform,
		Tag: app.DaemonRef("bld_test", "att_test1"), Secrets: map[string][]byte{"tok": []byte("s3cr3t-value")}}
	img, err := d.Build(ctx, req, log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out.String())
	}
	t.Logf("image %s %s", img.ID, img.Ref)
	found, err := d.FindAttemptImage(ctx, "bld_test", "att_test1")
	if err != nil || found == nil || found.ID != img.ID {
		t.Fatalf("find: %v %v", found, err)
	}
	if missing, _ := d.FindAttemptImage(ctx, "bld_test", "att_other"); missing != nil {
		t.Fatal("found an image for another attempt")
	}

	// Cancel a long build.
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:1.37\nRUN echo start-"+time.Now().String()+" && sleep 120\n"), 0o644)
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	req.AttemptID, req.Tag = "att_test2", app.DaemonRef("bld_test", "att_test2")
	_, err = d.Build(cctx, req, log)
	if err == nil {
		t.Fatal("cancelled build succeeded")
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("cancel took %s", time.Since(start))
	}
	t.Logf("cancelled after %s: %v", time.Since(start), err)
	if strings.Contains(out.String(), "s3cr3t-value") {
		t.Fatal("secret in build output")
	}
}
