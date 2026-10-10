package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// cursorManifest is .cursor-plugin/plugin.json (cursor/plugins
// schemas/plugin.schema.json). Each component is a path, a list of paths,
// or for mcpServers and hooks also an inline object; absent components are
// looked for in their default places.
type cursorManifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Skills      json.RawMessage `json:"skills"`
	Agents      json.RawMessage `json:"agents"`
	Commands    json.RawMessage `json:"commands"`
	Rules       json.RawMessage `json:"rules"`
	Hooks       json.RawMessage `json:"hooks"`
	MCPServers  json.RawMessage `json:"mcpServers"`
}

// cursorRoot is the plugin directory variable of Cursor plugins; it means
// the same as ${PLUGIN_ROOT}.
const cursorRoot = "${CURSOR_PLUGIN_ROOT}"

func readCursor(dir string, exclude []string) (*Plugin, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".cursor-plugin", "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m cursorManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf(".cursor-plugin/plugin.json: %w", err)
	}
	if !nameRE.MatchString(m.Name) {
		return nil, fmt.Errorf(".cursor-plugin/plugin.json: name %q must match %s", m.Name, nameRE)
	}
	p := &Plugin{Manifest: Manifest{Name: m.Name, Version: m.Version, Description: m.Description}, Dir: dir, Servers: map[string]Server{}, Format: "cursor"}
	use := func(c string) bool { return !slices.Contains(exclude, c) }

	// Components with no Codex form here: present ones must be excluded.
	for _, c := range []struct {
		name  string
		raw   json.RawMessage
		local string
	}{{ComponentAgents, m.Agents, "agents"}, {ComponentCommands, m.Commands, "commands"}, {ComponentRules, m.Rules, "rules"}, {ComponentHooks, m.Hooks, "hooks/hooks.json"}} {
		if use(c.name) && (len(c.raw) > 0 || exists(filepath.Join(dir, c.local))) {
			return nil, fmt.Errorf("%s: Cursor %s have no Codex equivalent in this builder; add %q to the plugin's exclude to build without them", m.Name, c.name, c.name)
		}
	}

	if use(ComponentSkills) {
		paths, err := paths(m.Skills, "skills")
		if err != nil {
			return nil, fmt.Errorf("%s: skills: %w", m.Name, err)
		}
		for _, rel := range paths {
			if rel, err = inside(rel); err != nil {
				return nil, fmt.Errorf("%s: skills: %w", m.Name, err)
			}
			skills, err := readSkills(dir, rel)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", m.Name, err)
			}
			p.Skills = append(p.Skills, skills...)
		}
	}

	if use(ComponentMCP) {
		if err := p.readCursorMCP(m.MCPServers); err != nil {
			return nil, fmt.Errorf("%s: mcpServers: %w", m.Name, err)
		}
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	return p, nil
}

// readCursorMCP reads mcpServers: a file path, an inline {"mcpServers":…}
// or server map, or a list of these; .mcp.json or mcp.json when absent.
func (p *Plugin) readCursorMCP(raw json.RawMessage) error {
	if len(raw) == 0 {
		for _, f := range []string{".mcp.json", "mcp.json"} {
			if exists(filepath.Join(p.Dir, f)) {
				raw, _ = json.Marshal(f)
				break
			}
		}
		if len(raw) == 0 {
			return nil
		}
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		list = []json.RawMessage{raw}
	}
	for _, item := range list {
		var file string
		if json.Unmarshal(item, &file) == nil {
			rel, err := inside(file)
			if err != nil {
				return err
			}
			if item, err = os.ReadFile(filepath.Join(p.Dir, filepath.FromSlash(rel))); err != nil {
				return err
			}
		}
		var wrapped struct {
			Servers json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(item, &wrapped) == nil && len(wrapped.Servers) > 0 {
			item = wrapped.Servers
		}
		// Cursor's plugin root variable is ours under another name.
		item = []byte(strings.ReplaceAll(string(item), cursorRoot, "${"+Root+"}"))
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(item, &servers); err != nil {
			return err
		}
		for name, s := range servers {
			var server Server
			d := json.NewDecoder(strings.NewReader(string(s)))
			d.DisallowUnknownFields()
			if err := d.Decode(&server); err != nil {
				return fmt.Errorf("server %s: %w (OAuth and other interactive settings are not supported)", name, err)
			}
			if _, dup := p.Servers[name]; dup {
				return fmt.Errorf("server %s is defined twice", name)
			}
			p.Servers[name] = server
		}
	}
	return nil
}

// paths reads a path-or-paths field; def when absent.
func paths(raw json.RawMessage, def string) ([]string, error) {
	if len(raw) == 0 {
		return []string{def}, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, errors.New("must be a path or a list of paths")
	}
	return many, nil
}

// inside cleans a plugin-relative path and rejects ones leaving the plugin.
func inside(rel string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(rel), "./"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "*?[") {
		return "", fmt.Errorf("%q must be a path inside the plugin", rel)
	}
	return clean, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
