package plan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akarachen/sandboxed-agent-builder/internal/plugin"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

type dirFetcher string

func (d dirFetcher) Fetch(_, _ string) (string, error) { return string(d), nil }

func TestWrite(t *testing.T) {
	repo := t.TempDir()
	pdir := filepath.Join(repo, "plugins", "probe")
	os.MkdirAll(filepath.Join(pdir, "skills", "one"), 0o755)
	os.WriteFile(filepath.Join(pdir, "plugin.json"), []byte(`{"name":"probe","version":"1","description":"d"}`), 0o644)
	os.WriteFile(filepath.Join(pdir, "skills", "one", "SKILL.md"), []byte("---\nname: one\ndescription: d\n---\n"), 0o644)
	digest, _ := plugin.ContentDigest(pdir)

	r := spec.Resolved{
		Contract: 1, Platform: "linux/amd64",
		Base:    spec.ResolvedImage{Family: spec.FamilyDebianSlim, Version: "trixie", Image: "docker.io/library/debian:trixie-slim", Digest: "sha256:base"},
		Node:    spec.ResolvedNode{Version: "24.1.0", URL: "https://nodejs.org/dist/v24.1.0/node.tar.gz", SHA256: "abc"},
		Agent:   spec.ResolvedAgent{Type: spec.AgentCodex, Codex: &spec.ResolvedPackage{Package: "@openai/codex", Version: "1.2.3"}},
		Oar:     spec.ResolvedPackage{Package: "@botiverse/oar-cli", Version: "0.51.0"},
		Akhook:  spec.ResolvedAkhook{Repository: "https://github.com/aghub-app/akhook", Commit: strings.Repeat("a", 40), Toolchain: spec.ResolvedImage{Image: "rust:1-alpine", Digest: "sha256:rust"}},
		Plugins: []spec.ResolvedPlugin{{Name: "probe", Source: "https://x", Commit: strings.Repeat("b", 40), Path: "plugins/probe", ContentDigest: digest}},
		Scripts: spec.Scripts{
			Build:   []spec.Script{{Name: "first", Run: "echo 1"}, {Name: "second", Run: "echo 2", Secrets: []string{"npm"}}},
			Startup: []spec.Script{{Name: "Prepare workspace!", Run: "echo start"}},
		},
	}
	dir := t.TempDir()
	p, err := Write(r, dirFetcher(repo), dir)
	if err != nil {
		t.Fatal(err)
	}
	df := p.Dockerfile
	// One RUN per build script, in order, the secret only on its own step.
	i1 := strings.Index(df, "source=build/001.sh")
	i2 := strings.Index(df, "source=build/002.sh")
	if i1 < 0 || i2 < i1 || strings.Count(df, "--mount=type=secret,id=npm") != 1 || strings.Index(df, "id=npm") < i2 {
		t.Fatalf("build scripts:\n%s", df)
	}
	// Startup scripts are copied, never run.
	if !strings.Contains(df, "COPY agent/startup /opt/agent/startup") || strings.Contains(df, "001-prepare-workspace.sh") {
		t.Fatalf("startup:\n%s", df)
	}
	for _, want := range []string{"FROM docker.io/library/debian:trixie-slim@sha256:base", "@openai/codex@1.2.3", "@botiverse/oar-cli@0.51.0",
		"ADD --checksum=sha256:abc", "COPY plugins/probe /opt/agent/plugins/probe", "COPY plugins/probe/skills/one /etc/codex/skills/probe--one",
		`ENTRYPOINT ["/usr/local/bin/agent-entrypoint"]`, "USER agent"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if strings.Index(df, "COPY plugins/probe ") > i1 {
		t.Error("plugins must come before build scripts")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agent/startup/001-prepare-workspace.sh")); !strings.Contains(string(b), "echo start") {
		t.Fatalf("startup file: %q", b)
	}
	if strings.Join(p.Secrets, ",") != "npm" {
		t.Fatalf("secrets %v", p.Secrets)
	}

	// A changed plugin no longer matches the locked digest.
	os.WriteFile(filepath.Join(pdir, "extra"), []byte("x"), 0o644)
	if _, err := Write(r, dirFetcher(repo), t.TempDir()); err == nil {
		t.Fatal("changed plugin accepted")
	}
}

// TestEntrypoint runs the generated entrypoint with a stub oar: startup
// scripts run in order before oar, and a failing one stops the container.
func TestEntrypoint(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "oar"), []byte("#!/bin/sh\necho \"oar $*\"\n"), 0o755)
	script := strings.ReplaceAll(entrypoint, "/opt/agent/startup", filepath.Join(root, "startup"))
	os.WriteFile(filepath.Join(root, "entrypoint"), []byte(script), 0o755)
	os.MkdirAll(filepath.Join(root, "startup"), 0o755)
	os.WriteFile(filepath.Join(root, "startup", "001-a.sh"), []byte("echo first\n"), 0o755)
	os.WriteFile(filepath.Join(root, "startup", "002-b.sh"), []byte("echo second\n"), 0o755)
	run := func() (string, error) {
		cmd := exec.Command(filepath.Join(root, "entrypoint"), "run", "codex", "hi")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + root}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err != nil || strings.Index(out, "first") > strings.Index(out, "second") || !strings.HasSuffix(strings.TrimSpace(out), "oar run codex hi") {
		t.Fatalf("%v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(root, "startup", "002-b.sh"), []byte("exit 3\n"), 0o755)
	out, err = run()
	if err == nil || strings.Contains(out, "oar run") || !strings.Contains(out, "Codex was not started") {
		t.Fatalf("failing startup: %v\n%s", err, out)
	}
}
