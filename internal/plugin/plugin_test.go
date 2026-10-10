package plugin

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	pluginSchema = `"$schema": "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json"`
	mcpSchema    = `"$schema": "https://agent-plugins.org/schemas/1.1.0/mcp.schema.json"`
)

func TestReadAndCompile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "probe")
	write(t, dir, map[string]string{
		"plugin.json": `{` + pluginSchema + `, "name": "acme.probe", "version": "1.0.0", "author": {"name": "x"}, "targets": ["chat"],
			"extensions": {"com.example.other": {"anything": [1]}}}`,
		"mcp.json": `{` + mcpSchema + `, "mcpServers": {
			"local": {"type": "stdio", "command": "./bin/server", "args": ["--data", "${PLUGIN_DATA}/db", "${HOME}"], "env": {"CONFIG": "${PLUGIN_ROOT}/c.json"}, "cwd": "${PLUGIN_DATA}"},
			"npx": {"type": "stdio", "command": "npx", "args": ["tool@1.0.0"]},
			"remote": {"type": "streamable-http", "url": "https://example.com/mcp", "headers": {"X-Tenant": "public"}},
			"legacy": {"type": "sse", "url": "https://example.com/sse"},
			"secret-env": {"type": "stdio", "command": "x", "env": {"PLUGIN_ROOT": "/"}},
			"mixed": {"type": "stdio", "command": "x", "url": "https://example.com"},
			"plain-http": {"type": "streamable-http", "url": "http://example.com/mcp"},
			"loopback": {"type": "streamable-http", "url": "http://127.0.0.1:9000/mcp"},
			"escape": {"type": "stdio", "command": "../bin/server"},
			"bad-cwd": {"type": "stdio", "command": "x", "cwd": "data"}}}`,
		"bin/server":                  "#!/bin/sh\n",
		"skills/one/SKILL.md":         "---\nname: one\ndescription: d\n---\nbody\n",
		"skills/mismatch/SKILL.md":    "---\nname: other\ndescription: d\n---\n",
		"skills/no-desc/SKILL.md":     "---\nname: no-desc\n---\n",
		"skills/notes/README.md":      "not a skill",
		"app.aghub.akhook/akhook.yml": "version: 1\nrules:\n  - id: r\n    on: prompt_submit\n    run:\n      argv: [\"${PLUGIN_ROOT}/hook\", \"${PLUGIN_DATA}\"]\n",
	})
	p, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "acme.probe" || p.SpecVersion != "1.1.0" || len(p.Skills) != 1 || p.Skills[0].Name != "one" {
		t.Fatalf("%+v", p)
	}
	servers := slices.Sorted(maps.Keys(p.Servers))
	if strings.Join(servers, ",") != "local,loopback,npx,remote" {
		t.Fatalf("servers %v", servers)
	}
	warnings := strings.Join(p.Warnings, "\n")
	for _, want := range []string{`unknown field "targets"`, "skills/mismatch: skipped", "skills/no-desc: skipped",
		"server legacy skipped", "server secret-env skipped", "server mixed skipped", "server plain-http skipped",
		"server escape skipped", "server bad-cwd skipped"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("no warning %q in:\n%s", want, warnings)
		}
	}
	if strings.Contains(warnings, "com.example.other") {
		t.Error("an unimplemented extension namespace was validated")
	}

	cfg, err := CodexConfig([]*Plugin{p})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[mcp_servers.acme_probe_local]", "command = '/opt/agent/plugins/acme.probe/bin/server'",
		"args = ['--data', '/var/lib/agent-plugins/acme.probe/db', '${HOME}']", "cwd = '/var/lib/agent-plugins/acme.probe'",
		"CONFIG = '/opt/agent/plugins/acme.probe/c.json'", "PLUGIN_DATA = '/var/lib/agent-plugins/acme.probe'", "PLUGIN_ROOT = '/opt/agent/plugins/acme.probe'",
		"[mcp_servers.acme_probe_npx]", "command = 'npx'", "cwd = '/opt/agent/plugins/acme.probe'",
		"[mcp_servers.acme_probe_remote]", "bearer_token_env_var = 'MCP_TOKEN_ACME_PROBE_REMOTE'", "X-Tenant = 'public'",
		"default_tools_approval_mode = 'approve'",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	if env := RuntimeEnv(p); strings.Join(env, ",") != "MCP_TOKEN_ACME_PROBE_LOOPBACK,MCP_TOKEN_ACME_PROBE_REMOTE" {
		t.Fatalf("runtime env %v", env)
	}
	hooks, err := HooksYAML([]*Plugin{p})
	if err != nil || !strings.Contains(hooks, "id: acme.probe/r") || !strings.Contains(hooks, "/opt/agent/plugins/acme.probe/hook") || !strings.Contains(hooks, "/var/lib/agent-plugins/acme.probe") {
		t.Fatalf("%v\n%s", err, hooks)
	}
}

func TestManifestRejections(t *testing.T) {
	for name, manifest := range map[string]string{
		"no schema":      `{"name": "p"}`,
		"unknown schema": `{"$schema": "https://agent-plugins.org/schemas/9.0.0/plugin.schema.json", "name": "p"}`,
		"no name":        `{` + pluginSchema + `}`,
		"bad name":       `{` + pluginSchema + `, "name": "Bad--Name"}`,
		"version type":   `{` + pluginSchema + `, "name": "p", "version": 1}`,
		"author field":   `{` + pluginSchema + `, "name": "p", "author": {"name": "x", "twitter": "y"}}`,
		"keywords type":  `{` + pluginSchema + `, "name": "p", "keywords": "x"}`,
		"not an object":  `[]`,
	} {
		dir := filepath.Join(t.TempDir(), "p")
		write(t, dir, map[string]string{"plugin.json": manifest})
		if _, err := Read(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMCPDisabledAsAWhole(t *testing.T) {
	for name, mcp := range map[string]string{
		"version mismatch": `{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json", "mcpServers": {"a": {"type": "stdio", "command": "x"}}}`,
		"extra field":      `{` + mcpSchema + `, "mcpServers": {}, "other": 1}`,
		"no servers":       `{` + mcpSchema + `}`,
	} {
		dir := filepath.Join(t.TempDir(), "p")
		write(t, dir, map[string]string{"plugin.json": `{` + pluginSchema + `, "name": "p"}`, "mcp.json": mcp,
			"skills/s/SKILL.md": "---\nname: s\ndescription: d\n---\n"})
		p, err := Read(dir)
		if err != nil || len(p.Servers) != 0 || len(p.Skills) != 1 || !strings.Contains(strings.Join(p.Warnings, ""), "MCP disabled") {
			t.Errorf("%s: %+v %v", name, p, err)
		}
	}
}

func TestPathsStayInsideThePlugin(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, map[string]string{"SKILL.md": "---\nname: leak\ndescription: d\n---\n"})
	dir := filepath.Join(t.TempDir(), "p")
	write(t, dir, map[string]string{"plugin.json": `{` + pluginSchema + `, "name": "p"}`})
	os.MkdirAll(filepath.Join(dir, "skills"), 0o755)
	if err := os.Symlink(outside, filepath.Join(dir, "skills", "leak")); err != nil {
		t.Fatal(err)
	}
	p, err := Read(dir)
	if err != nil || len(p.Skills) != 0 {
		t.Fatalf("a skill outside the plugin was read: %+v %v", p, err)
	}
}

func TestCheckSetSkipsSkillClash(t *testing.T) {
	a := &Plugin{Name: "a", Skills: []Skill{{Dir: "same", Name: "same"}}}
	b := &Plugin{Name: "b", Skills: []Skill{{Dir: "same", Name: "same"}}}
	if err := CheckSet([]*Plugin{a, b}); err != nil || len(b.Skills) != 0 || len(b.Warnings) != 1 {
		t.Fatalf("%v %+v", err, b)
	}
	if err := CheckSet([]*Plugin{a, {Name: "a"}}); err == nil {
		t.Fatal("duplicate plugin names accepted")
	}
}
