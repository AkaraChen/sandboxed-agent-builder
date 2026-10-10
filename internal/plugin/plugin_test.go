package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		mode := os.FileMode(0o644)
		if name == "setup" {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

var probe = map[string]string{
	"plugin.json": `{"name":"probe","version":"1.0.0","description":"d"}`,
	"mcp.json": `{"mcpServers":{
		"local":{"command":"node","args":["${PLUGIN_ROOT}/s.mjs"],"env":{"TOKEN":"${PROBE_KEY}","SAME":"${SAME}","MODE":"x"}},
		"remote":{"url":"https://example.com/mcp","headers":{"Authorization":"Bearer ${REMOTE_KEY}","X-Team":"${TEAM}","X-Static":"v"}}}}`,
	"skills/one/SKILL.md": "---\nname: probe-one\ndescription: d\n---\nbody\n",
	"akhook.yml":          "version: 1\nrules:\n  - id: r\n    on: prompt_submit\n    run:\n      argv: [\"${PLUGIN_ROOT}/hook\"]\n",
	"setup":               "#!/bin/sh\n",
}

func TestReadAndCompile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "probe")
	write(t, dir, probe)
	p, err := Read(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Setup || len(p.Skills) != 1 || p.Skills[0].Name != "probe-one" || strings.Join(p.RuntimeEnv, ",") != "PROBE_KEY,REMOTE_KEY,SAME,TEAM" {
		t.Fatalf("%+v", p)
	}
	cfg, err := CodexConfig([]*Plugin{p})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[mcp_servers.probe_local]", `command = '/bin/sh'`, `export TOKEN="$PROBE_KEY"`, "/opt/agent/plugins/probe/s.mjs",
		"env_vars = ['PROBE_KEY', 'SAME']", "MODE = 'x'", "default_tools_approval_mode = 'approve'",
		"[mcp_servers.probe_remote]", "bearer_token_env_var = 'REMOTE_KEY'", "X-Team = 'TEAM'", "X-Static = 'v'",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
	hooks, err := HooksYAML([]*Plugin{p})
	if err != nil || !strings.Contains(hooks, "id: probe/r") || !strings.Contains(hooks, "/opt/agent/plugins/probe/hook") {
		t.Fatalf("%v\n%s", err, hooks)
	}
}

func TestRejections(t *testing.T) {
	cases := map[string]map[string]string{
		"runtime var in url":        {"mcp.json": `{"mcpServers":{"s":{"url":"${GATEWAY}/mcp"}}}`},
		"partial env value":         {"mcp.json": `{"mcpServers":{"s":{"command":"x","env":{"A":"pre-${KEY}"}}}}`},
		"command and url":           {"mcp.json": `{"mcpServers":{"s":{"command":"x","url":"https://x"}}}`},
		"claude hooks":              {"hooks/hooks.json": `{}`},
		"hook event":                {"akhook.yml": "version: 1\nrules:\n  - id: r\n    on: session_start\n"},
		"skill without frontmatter": {"skills/a/SKILL.md": "no frontmatter"},
		"bad name":                  {"plugin.json": `{"name":"Bad Name","version":"1","description":"d"}`},
	}
	for name, files := range cases {
		dir := filepath.Join(t.TempDir(), "p")
		write(t, dir, map[string]string{"plugin.json": `{"name":"p","version":"1","description":"d"}`})
		write(t, dir, files)
		if _, err := Read(dir, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckSetRejectsSkillClash(t *testing.T) {
	a := &Plugin{Manifest: Manifest{Name: "a"}, Skills: []Skill{{Dir: "x", Name: "same"}}}
	b := &Plugin{Manifest: Manifest{Name: "b"}, Skills: []Skill{{Dir: "y", Name: "same"}}}
	if err := CheckSet([]*Plugin{a, b}); err == nil {
		t.Fatal("clash accepted")
	}
}

func TestCursorPlugin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kit")
	write(t, dir, map[string]string{
		".cursor-plugin/plugin.json": `{"name":"kit","displayName":"Kit","version":"1.2.0","description":"d","author":{"name":"x"},
			"skills":["./skills/","./extra/solo"],"agents":"./agents/",
			"mcpServers":{"docs":{"type":"http","url":"https://example.com/mcp","headers":{"Authorization":"Bearer ${DOCS_TOKEN}"}},
			              "local":{"command":"node","args":["${CURSOR_PLUGIN_ROOT}/s.mjs"]}}}`,
		"skills/one/SKILL.md": "---\nname: kit-one\ndescription: d\n---\n",
		"skills/two/SKILL.md": "---\nname: kit-two\ndescription: d\n---\n",
		"extra/solo/SKILL.md": "---\nname: kit-solo\ndescription: d\n---\n",
		"agents/helper.md":    "---\nname: helper\n---\n",
	})
	if _, err := Read(dir, nil); err == nil || !strings.Contains(err.Error(), `add "agents" to the plugin's exclude`) {
		t.Fatalf("agents without exclude: %v", err)
	}
	p, err := Read(dir, []string{"agents"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, s := range p.Skills {
		paths = append(paths, s.Path+"="+s.Name)
	}
	if p.Format != "cursor" || strings.Join(paths, ",") != "skills/one=kit-one,skills/two=kit-two,extra/solo=kit-solo" || strings.Join(p.RuntimeEnv, ",") != "DOCS_TOKEN" {
		t.Fatalf("%+v %v", p, paths)
	}
	cfg, err := CodexConfig([]*Plugin{p})
	if err != nil || !strings.Contains(cfg, "/opt/agent/plugins/kit/s.mjs") || !strings.Contains(cfg, "bearer_token_env_var = 'DOCS_TOKEN'") {
		t.Fatalf("%v\n%s", err, cfg)
	}
	if p, err := Read(dir, []string{"agents", "skills", "mcpServers"}); err != nil || len(p.Skills) != 0 || len(p.Servers) != 0 {
		t.Fatalf("excluded: %+v %v", p, err)
	}

	write(t, dir, map[string]string{".cursor-plugin/plugin.json": `{"name":"kit","version":"1","mcpServers":"./mcp.json"}`,
		"mcp.json": `{"mcpServers":{"crm":{"type":"http","url":"https://crm.example/mcp","auth":{"CLIENT_ID":"${CLIENT_ID}"}}}}`})
	if _, err := Read(dir, []string{"agents"}); err == nil || !strings.Contains(err.Error(), "OAuth") {
		t.Fatalf("oauth server: %v", err)
	}
	write(t, dir, map[string]string{".cursor-plugin/plugin.json": `{"name":"kit","version":"1","skills":"../outside"}`})
	if _, err := Read(dir, []string{"agents"}); err == nil {
		t.Fatal("skills path outside the plugin accepted")
	}
}
