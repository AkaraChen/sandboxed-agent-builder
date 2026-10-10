// Package plugin reads Agent Plugins (https://agent-plugins.org, versions
// 1.0.0 and 1.1.0, which are normatively identical) and compiles them to
// Codex's native configuration.
//
//	plugin.json         manifest: $schema, name and metadata
//	skills/<name>/      Agent Skills
//	mcp.json            MCP servers: stdio, streamable-http (sse is skipped)
//	app.aghub.akhook/   this client's extension: akhook.yml hook rules
//
// Following the specification, a manifest violation rejects the plugin,
// while an invalid skill, MCP configuration or server entry is skipped and
// reported as a warning. Hooks are not part of the portable format; akhook
// runs them inside the image and the builder only merges the plugins' rules.
package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Recognized schema identifiers, by specification version.
var (
	pluginSchemas = map[string]string{
		"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json": "1.0.0",
		"https://agent-plugins.org/schemas/1.1.0/plugin.schema.json": "1.1.0",
	}
	mcpSchemas = map[string]string{
		"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json": "1.0.0",
		"https://agent-plugins.org/schemas/1.1.0/mcp.schema.json": "1.1.0",
	}
)

// AkhookNamespace is the client extension directory holding a plugin's
// akhook rules (akhook.yml).
const AkhookNamespace = "app.aghub.akhook"

// Server is an MCP server entry: stdio (Command) or streamable-http (URL).
type Server struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Skill is skills/<Dir>/SKILL.md; its name equals Dir.
type Skill struct {
	Dir  string
	Name string
}

// Plugin is a read plugin directory.
type Plugin struct {
	Name        string
	Version     string
	Description string
	// SpecVersion is the Agent Plugins version plugin.json declares.
	SpecVersion string
	Dir         string
	Servers     map[string]Server
	Skills      []Skill
	// Hooks is the akhook rules from the extension directory.
	Hooks []map[string]any
	// Warnings report what was ignored or skipped.
	Warnings []string
}

// Placeholders the specification expands in stdio args, env and cwd.
const (
	Root = "${PLUGIN_ROOT}"
	Data = "${PLUGIN_DATA}"
)

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,62}[a-z0-9])?$`)
	skillNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	headerRE    = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// Hook events akhook runs, by the akhook rule's `on`.
var (
	LifecycleEvents = []string{"prompt_submit", "stop"}
	ToolEvents      = []string{"shell_exec", "file_change", "tool_call"}
)

// Read reads and checks the plugin in dir.
func Read(dir string) (*Plugin, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	p := &Plugin{Dir: dir, Servers: map[string]Server{}}
	if err := p.readManifest(root); err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	p.readSkills(root)
	p.readMCP(root)
	if p.Hooks, err = readHooks(root); err != nil {
		return nil, fmt.Errorf("%s: %s/akhook.yml: %w", p.Name, AkhookNamespace, err)
	}
	return p, nil
}

func (p *Plugin) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// within resolves rel, a plugin-relative path, and reports whether it is
// inside root.
func within(root, rel string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	r, err := filepath.Rel(root, resolved)
	return resolved, err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

func (p *Plugin) readManifest(root string) error {
	file, ok := within(root, "plugin.json")
	if !ok {
		return errors.New("missing, or resolves outside the plugin")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}
	str := func(field string, required bool) (string, error) {
		raw, ok := m[field]
		if !ok {
			if required {
				return "", fmt.Errorf("%s is required", field)
			}
			return "", nil
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", fmt.Errorf("%s must be a string", field)
		}
		return s, nil
	}
	schema, err := str("$schema", true)
	if err != nil {
		return err
	}
	if p.SpecVersion = pluginSchemas[schema]; p.SpecVersion == "" {
		return fmt.Errorf("unsupported $schema %q (Agent Plugins 1.0.0 and 1.1.0 are supported)", schema)
	}
	if p.Name, err = str("name", true); err != nil {
		return err
	}
	if !nameRE.MatchString(p.Name) || strings.Contains(p.Name, "--") || strings.Contains(p.Name, "..") {
		return fmt.Errorf("name %q: 1-64 of a-z, 0-9, - and ., alphanumeric at both ends, without doubled - or . characters", p.Name)
	}
	if p.Version, err = str("version", false); err != nil {
		return err
	}
	if p.Description, err = str("description", false); err != nil {
		return err
	}
	for _, f := range []string{"homepage", "repository", "license"} {
		if _, err := str(f, false); err != nil {
			return err
		}
	}
	if raw, ok := m["keywords"]; ok {
		var k []string
		if json.Unmarshal(raw, &k) != nil {
			return errors.New("keywords must be an array of strings")
		}
	}
	if raw, ok := m["author"]; ok {
		var a map[string]json.RawMessage
		if json.Unmarshal(raw, &a) != nil {
			return errors.New("author must be an object")
		}
		for k, v := range a {
			var s string
			if !slices.Contains([]string{"name", "email", "url"}, k) || json.Unmarshal(v, &s) != nil {
				return fmt.Errorf("author.%s: author has only string name, email and url", k)
			}
		}
	}
	if raw, ok := m["extensions"]; ok {
		var e map[string]json.RawMessage
		if json.Unmarshal(raw, &e) != nil {
			p.warn("plugin.json: extensions is not an object; ignored")
		}
	}
	known := []string{"$schema", "name", "version", "description", "author", "homepage", "repository", "license", "keywords", "extensions"}
	for k := range m {
		if !slices.Contains(known, k) {
			p.warn("plugin.json: unknown field %q ignored", k)
		}
	}
	return nil
}

func (p *Plugin) readSkills(root string) {
	if _, err := os.Lstat(filepath.Join(root, "skills")); err != nil {
		return
	}
	dir, ok := within(root, "skills")
	if !ok {
		p.warn("skills: resolves outside the plugin; skills disabled")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.warn("skills: not a directory; skills disabled")
		return
	}
	for _, e := range entries {
		file, ok := within(root, path.Join("skills", e.Name(), "SKILL.md"))
		if !ok {
			continue
		}
		if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := checkSkill(file, e.Name()); err != nil {
			p.warn("skills/%s: skipped: %v", e.Name(), err)
			continue
		}
		p.Skills = append(p.Skills, Skill{Dir: e.Name(), Name: e.Name()})
	}
}

// checkSkill checks SKILL.md's frontmatter against the Agent Skills
// specification: a name matching its directory and a description.
func checkSkill(file, dir string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return errors.New("SKILL.md needs YAML frontmatter")
	}
	var front strings.Builder
	closed := false
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "---" {
			closed = true
			break
		}
		front.WriteString(sc.Text() + "\n")
	}
	if !closed {
		return errors.New("SKILL.md frontmatter is not closed")
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front.String()), &meta); err != nil {
		return fmt.Errorf("frontmatter: %w", err)
	}
	if meta.Name != dir || !skillNameRE.MatchString(meta.Name) || strings.Contains(meta.Name, "--") {
		return fmt.Errorf("name %q must be the directory name, of a-z, 0-9 and single -", meta.Name)
	}
	if meta.Description == "" || len(meta.Description) > 1024 {
		return errors.New("description must be 1-1024 characters")
	}
	return nil
}

func (p *Plugin) readMCP(root string) {
	if _, err := os.Lstat(filepath.Join(root, "mcp.json")); err != nil {
		return
	}
	disable := func(format string, args ...any) { p.warn("mcp.json: "+format+"; MCP disabled", args...) }
	file, ok := within(root, "mcp.json")
	if !ok {
		disable("resolves outside the plugin")
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		disable("%v", err)
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		disable("not a JSON object")
		return
	}
	var schema string
	_ = json.Unmarshal(top["$schema"], &schema)
	switch v := mcpSchemas[schema]; {
	case v == "":
		disable("unsupported $schema %q", schema)
		return
	case v != p.SpecVersion:
		disable("$schema targets %s but plugin.json targets %s", v, p.SpecVersion)
		return
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(top["mcpServers"], &servers); err != nil || servers == nil || len(top) != 2 {
		disable("needs exactly $schema and an mcpServers object")
		return
	}
	for name, raw := range servers {
		s, err := readServer(root, raw)
		if err != nil {
			p.warn("mcp.json: server %s skipped: %v", name, err)
			continue
		}
		p.Servers[name] = s
	}
}

func readServer(root string, raw json.RawMessage) (Server, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Server{}, errors.New("not an object")
	}
	var s Server
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, err
	}
	allowed := map[string][]string{
		"stdio":           {"type", "command", "args", "env", "cwd"},
		"streamable-http": {"type", "url", "headers"},
		"sse":             {"type", "url", "headers"},
	}[s.Type]
	if allowed == nil {
		return s, fmt.Errorf("unknown type %q", s.Type)
	}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			return s, fmt.Errorf("%s does not belong to a %s server", k, s.Type)
		}
	}
	switch s.Type {
	case "sse":
		return s, errors.New("the legacy HTTP+SSE transport is not supported")
	case "stdio":
		return s, checkStdio(root, s)
	}
	return s, checkHTTP(s)
}

func checkStdio(root string, s Server) error {
	if s.Command == "" || strings.ContainsAny(s.Command, " \t\n") {
		return errors.New("command must be one executable token")
	}
	if rel, ok := strings.CutPrefix(s.Command, "./"); ok {
		if _, ok := within(root, rel); !ok {
			return fmt.Errorf("command %s is not inside the plugin", s.Command)
		}
	} else if strings.Contains(s.Command, "/") {
		return fmt.Errorf("command %s must be a bare name or start with ./", s.Command)
	}
	for k := range s.Env {
		if k == "PLUGIN_ROOT" || k == "PLUGIN_DATA" {
			return fmt.Errorf("env must not set %s", k)
		}
	}
	switch {
	case s.Cwd == "":
	case strings.HasPrefix(s.Cwd, "./"):
		if _, ok := within(root, s.Cwd[2:]); !ok {
			return fmt.Errorf("cwd %s is not inside the plugin", s.Cwd)
		}
	case s.Cwd == Root || strings.HasPrefix(s.Cwd, Root+"/"):
		if _, ok := within(root, strings.TrimPrefix(s.Cwd[len(Root):], "/")); !ok {
			return fmt.Errorf("cwd %s is not inside the plugin", s.Cwd)
		}
	case s.Cwd == Data || strings.HasPrefix(s.Cwd, Data+"/"):
		if c := path.Clean(s.Cwd); c != Data && !strings.HasPrefix(c, Data+"/") {
			return fmt.Errorf("cwd %s leaves ${PLUGIN_DATA}", s.Cwd)
		}
	default:
		return fmt.Errorf("cwd %q must start with ./, ${PLUGIN_ROOT} or ${PLUGIN_DATA}", s.Cwd)
	}
	return nil
}

func checkHTTP(s Server) error {
	u, err := url.Parse(s.URL)
	if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http(s) URL", s.URL)
	}
	if u.User != nil || strings.Contains(s.URL, "#") {
		return errors.New("url must not have user information or a fragment")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("a non-loopback url must use https")
		}
	}
	seen := map[string]bool{}
	for k, v := range s.Headers {
		if !headerRE.MatchString(k) || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("header %q is not a valid HTTP header", k)
		}
		if seen[strings.ToLower(k)] {
			return fmt.Errorf("header %s is given twice", k)
		}
		seen[strings.ToLower(k)] = true
	}
	return nil
}

// readHooks reads AkhookNamespace/akhook.yml: version 1 and rules, each
// with an id and an event akhook runs. The rest of a rule is akhook's to
// check.
func readHooks(root string) ([]map[string]any, error) {
	rel := AkhookNamespace + "/akhook.yml"
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		return nil, nil
	}
	file, ok := within(root, rel)
	if !ok {
		return nil, errors.New("resolves outside the plugin")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Version int              `yaml:"version"`
		Rules   []map[string]any `yaml:"rules"`
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Version != 1 {
		return nil, errors.New("version must be 1")
	}
	ids := map[string]bool{}
	for i, r := range doc.Rules {
		id, _ := r["id"].(string)
		on, _ := r["on"].(string)
		if id == "" || ids[id] {
			return nil, fmt.Errorf("rules[%d]: needs a unique id", i)
		}
		ids[id] = true
		if !slices.Contains(LifecycleEvents, on) && !slices.Contains(ToolEvents, on) {
			return nil, fmt.Errorf("rule %s: on %q is not one of %v %v", id, on, LifecycleEvents, ToolEvents)
		}
	}
	return doc.Rules, nil
}

// CheckSet checks plugins that go into one image together: names must be
// unique; a skill whose name another plugin already uses is skipped with a
// warning, since Codex identifies skills by name.
func CheckSet(list []*Plugin) error {
	names := map[string]bool{}
	skills := map[string]string{}
	for _, p := range list {
		if names[p.Name] {
			return fmt.Errorf("two plugins are named %s", p.Name)
		}
		names[p.Name] = true
		kept := p.Skills[:0]
		for _, s := range p.Skills {
			if other, ok := skills[s.Name]; ok {
				p.warn("skills/%s: skipped: plugin %s already has a skill named %s", s.Dir, other, s.Name)
				continue
			}
			skills[s.Name] = p.Name
			kept = append(kept, s)
		}
		p.Skills = kept
		sort.Strings(p.Warnings)
	}
	return nil
}
