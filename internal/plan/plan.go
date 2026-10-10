// Package plan turns a resolved spec into a build context: a Dockerfile and
// the files it copies. The image is assembled in a fixed order so earlier
// layers stay cached when later inputs change:
//
//	base system → Node.js → akhook → Codex and oar → entrypoint and managed
//	hooks → one layer per plugin (sorted by name) → plugin configuration →
//	one RUN per build script → startup scripts and metadata
package plan

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akarachen/sandboxed-agent-builder/internal/plugin"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
	agentserver "github.com/akarachen/sandboxed-agent-builder/runtime/agent-server"
)

// Contract is the runtime contract version the generated image follows
// (docs/runtime-contract.md).
const Contract = 1

// Fetcher gives a plugin's directory at a locked commit.
type Fetcher interface {
	// Fetch returns a directory holding the repository at commit; the
	// caller must not change it.
	Fetch(source, commit string) (string, error)
}

// Plan is a written build context.
type Plan struct {
	Dir        string
	Dockerfile string
	// Secrets are the build secret ids the Dockerfile mounts.
	Secrets []string
}

// Write writes r's build context into dir, which must be empty.
func Write(r spec.Resolved, fetch Fetcher, dir string) (*Plan, error) {
	if r.Agent.Type != spec.AgentCodex || r.Agent.Codex == nil {
		return nil, fmt.Errorf("agent %q: only Codex images can be planned", r.Agent.Type)
	}
	var plugins []*plugin.Plugin
	for _, rp := range r.Plugins {
		repo, err := fetch.Fetch(rp.Source, rp.Commit)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", rp.Name, err)
		}
		src := filepath.Join(repo, filepath.FromSlash(rp.Path))
		p, err := plugin.Read(src, rp.Exclude)
		if err != nil {
			return nil, err
		}
		if p.Name != rp.Name {
			return nil, fmt.Errorf("plugin %s: name changed to %s", rp.Name, p.Name)
		}
		if err := copyTree(src, filepath.Join(dir, "plugins", p.Name)); err != nil {
			return nil, err
		}
		plugins = append(plugins, p)
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	if err := plugin.CheckSet(plugins); err != nil {
		return nil, err
	}

	config, err := plugin.CodexConfig(plugins)
	if err != nil {
		return nil, err
	}
	hooks, err := plugin.HooksYAML(plugins)
	if err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(Manifest(r), "", "  ")
	if err != nil {
		return nil, err
	}
	files := map[string]string{
		"agent/entrypoint.sh":     entrypoint,
		"agent/manifest.json":     string(manifest) + "\n",
		"codex/config.toml":       config,
		"codex/requirements.toml": plugin.CodexRequirements,
		"akhook/plugins.yml":      hooks,
	}
	for i, s := range r.Scripts.Build {
		files[fmt.Sprintf("build/%03d.sh", i+1)] = scriptFile(s)
	}
	for i, s := range r.Scripts.Startup {
		files[fmt.Sprintf("agent/startup/%03d-%s.sh", i+1, slug(s.Name))] = scriptFile(s)
	}
	if len(r.Scripts.Startup) == 0 {
		files["agent/startup/.keep"] = ""
	}
	if err := fs.WalkDir(agentserver.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := agentserver.Files.ReadFile(path)
		files["agent-server/"+path] = string(data)
		return err
	}); err != nil {
		return nil, err
	}
	df, secrets := dockerfile(r, plugins)
	files["Dockerfile"] = df
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			return nil, err
		}
	}
	return &Plan{Dir: dir, Dockerfile: df, Secrets: secrets}, nil
}

// ImageManifest is /opt/agent/manifest.json: what the image was built from.
type ImageManifest struct {
	Contract   int                   `json:"contract"`
	Platform   string                `json:"platform"`
	Base       spec.ResolvedImage    `json:"base"`
	Node       string                `json:"node"`
	Agent      spec.ResolvedAgent    `json:"agent"`
	Oar        string                `json:"oar"`
	Akhook     string                `json:"akhook"`
	Plugins    []spec.ResolvedPlugin `json:"plugins"`
	Startup    []string              `json:"startup"`
	RuntimeEnv []string              `json:"runtimeEnv"`
	ControlAPI ControlAPI            `json:"controlApi"`
}

// ControlAPI describes the agent control API the image serves with
// `agent-entrypoint serve` (docs/agent-api.md).
type ControlAPI struct {
	Version int `json:"version"`
	Port    int `json:"port"`
}

// ControlAPIPort is the control API's default port in the container.
const ControlAPIPort = 8080

func Manifest(r spec.Resolved) ImageManifest {
	m := ImageManifest{
		Contract: Contract, Platform: r.Platform, Base: r.Base,
		Node: r.Node.Version, Agent: r.Agent, Oar: r.Oar.Version, Akhook: r.Akhook.Commit,
		Plugins: r.Plugins, RuntimeEnv: r.RuntimeEnv, Startup: []string{},
		ControlAPI: ControlAPI{Version: 1, Port: ControlAPIPort},
	}
	for _, s := range r.Scripts.Startup {
		m.Startup = append(m.Startup, s.Name)
	}
	if m.Plugins == nil {
		m.Plugins = []spec.ResolvedPlugin{}
	}
	if m.RuntimeEnv == nil {
		m.RuntimeEnv = []string{}
	}
	return m
}

func scriptFile(s spec.Script) string {
	run := s.Run
	if !strings.HasSuffix(run, "\n") {
		run += "\n"
	}
	return fmt.Sprintf("#!/bin/sh\n# %s\n%s", oneLine(s.Name), run)
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slug(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "script"
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// copyTree copies src to dst, skipping .git; symlinks are kept as links.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			return os.WriteFile(target, data, mode)
		}
	})
}
