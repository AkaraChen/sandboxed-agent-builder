// Package plugin reads Agent Plugins and compiles them to Codex's native
// configuration.
//
// An Agent Plugin is a directory no agent harness owns (the standard of
// AXP's packages/plugins-go):
//
//	plugin.json       name, version, description
//	mcp.json          MCP servers, in the standard mcpServers shape
//	skills/*/SKILL.md Agent Skills
//	akhook.yml        hooks, as akhook rules
//	setup             run once while the image is built
//
// Hooks are not compiled here: akhook runs them inside the image (it holds
// the Codex hook adapter), and the builder only merges the plugins' rules
// into one akhook config.
package plugin

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is plugin.json.
type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Targets     []string `json:"targets,omitempty"`
}

// Server is an MCP server: a command (stdio) or a URL (streamable HTTP).
type Server struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Skill is one skills/<dir>/SKILL.md.
type Skill struct {
	Dir  string // directory name under skills/
	Name string // the frontmatter name, which Codex identifies it by
}

// Plugin is a read and checked plugin directory.
type Plugin struct {
	Manifest
	Dir     string
	Servers map[string]Server
	Skills  []Skill
	// Hooks is akhook.yml's rules, when there is one.
	Hooks []map[string]any
	Setup bool
	// RuntimeEnv is the environment variables the plugin's MCP servers read
	// when the agent runs.
	RuntimeEnv []string
}

// Root is the variable for the plugin's directory in the image; it is the
// only one expanded while building.
const Root = "PLUGIN_ROOT"

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	variableRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	wholeVarRE = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	bearerRE   = regexp.MustCompile(`^Bearer \$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	simpleRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Hook events akhook runs, by the akhook rule's `on`.
var (
	LifecycleEvents = []string{"prompt_submit", "stop"}
	ToolEvents      = []string{"shell_exec", "file_change", "tool_call"}
)

// Read reads and checks the plugin in dir.
func Read(dir string) (*Plugin, error) {
	p := &Plugin{Dir: dir, Servers: map[string]Server{}}
	if err := readJSON(filepath.Join(dir, "plugin.json"), &p.Manifest); err != nil {
		return nil, err
	}
	if !nameRE.MatchString(p.Name) {
		return nil, fmt.Errorf("plugin.json: name %q must match %s", p.Name, nameRE)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks", "hooks.json")); err == nil {
		return nil, fmt.Errorf("%s: hooks/hooks.json is not supported; a plugin's hooks are akhook rules in akhook.yml", p.Name)
	}

	var mcp struct {
		Servers map[string]Server `json:"mcpServers"`
	}
	switch err := readJSON(filepath.Join(dir, "mcp.json"), &mcp); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		p.Servers = mcp.Servers
	}
	env := map[string]bool{}
	for name, s := range p.Servers {
		if !simpleRE.MatchString(name) {
			return nil, fmt.Errorf("%s: server name %q", p.Name, name)
		}
		names, err := checkServer(s)
		if err != nil {
			return nil, fmt.Errorf("%s: server %s: %w", p.Name, name, err)
		}
		for _, n := range names {
			env[n] = true
		}
	}
	for n := range env {
		p.RuntimeEnv = append(p.RuntimeEnv, n)
	}
	sort.Strings(p.RuntimeEnv)

	skills, err := readSkills(filepath.Join(dir, "skills"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	p.Skills = skills

	if p.Hooks, err = readHooks(filepath.Join(dir, "akhook.yml")); err != nil {
		return nil, fmt.Errorf("%s: akhook.yml: %w", p.Name, err)
	}

	if info, err := os.Stat(filepath.Join(dir, "setup")); err == nil {
		if info.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("%s: setup is not executable", p.Name)
		}
		p.Setup = true
	}
	return p, nil
}

// checkServer checks where s uses variables and returns the runtime ones.
// ${PLUGIN_ROOT} may appear anywhere. Other variables are read from the
// agent's environment, which Codex can only do for a whole env value, a
// bearer token or a whole header value.
func checkServer(s Server) ([]string, error) {
	if (s.Command == "") == (s.URL == "") {
		return nil, errors.New("needs a command or a url")
	}
	onlyRoot := append([]string{s.Command, s.Cwd, s.URL}, s.Args...)
	for _, text := range onlyRoot {
		for _, m := range variableRE.FindAllStringSubmatch(text, -1) {
			if m[1] != Root {
				return nil, fmt.Errorf("${%s}: only ${%s} is expanded in command, args, cwd and url; runtime values go in env or headers", m[1], Root)
			}
		}
	}
	var names []string
	runtime := func(where, value string, allowBearer bool) error {
		if !strings.Contains(value, "${") {
			return nil
		}
		if m := wholeVarRE.FindStringSubmatch(value); m != nil && m[1] != Root {
			names = append(names, m[1])
			return nil
		}
		if m := bearerRE.FindStringSubmatch(value); allowBearer && m != nil && m[1] != Root {
			names = append(names, m[1])
			return nil
		}
		if rest := strings.ReplaceAll(value, "${"+Root+"}", ""); !strings.Contains(rest, "${") {
			return nil
		}
		return fmt.Errorf("%s: a runtime variable must be the whole value (or \"Bearer ${NAME}\" for Authorization)", where)
	}
	for k, v := range s.Env {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("env %q is not a variable name", k)
		}
		if err := runtime("env "+k, v, false); err != nil {
			return nil, err
		}
	}
	for k, v := range s.Headers {
		if err := runtime("header "+k, v, strings.EqualFold(k, "Authorization")); err != nil {
			return nil, err
		}
	}
	if s.URL != "" && len(s.Env) > 0 {
		return nil, errors.New("env is for stdio servers")
	}
	if s.Command != "" && len(s.Headers) > 0 {
		return nil, errors.New("headers are for url servers")
	}
	return names, nil
}

func readSkills(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !simpleRE.MatchString(e.Name()) {
			return nil, fmt.Errorf("skills/%s: directory names are letters, digits, _ and -", e.Name())
		}
		file := filepath.Join(dir, e.Name(), "SKILL.md")
		name, err := skillName(file)
		if err != nil {
			return nil, fmt.Errorf("skills/%s: %w", e.Name(), err)
		}
		out = append(out, Skill{Dir: e.Name(), Name: name})
	}
	return out, nil
}

// skillName reads the name from SKILL.md's YAML frontmatter.
func skillName(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", errors.New("SKILL.md needs YAML frontmatter")
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
		return "", errors.New("SKILL.md frontmatter is not closed")
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(front.String()), &meta); err != nil {
		return "", fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	if meta.Name == "" || meta.Description == "" {
		return "", errors.New("SKILL.md frontmatter needs name and description")
	}
	return meta.Name, nil
}

// readHooks reads akhook.yml: version 1 and rules, each with an id and an
// event akhook runs. The rest of a rule is akhook's to check, which the
// image build does by running akhook on the merged config.
func readHooks(file string) ([]map[string]any, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
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

func readJSON(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// ContentDigest hashes the files under dir (paths, executable bits and
// contents), skipping .git.
func ContentDigest(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "L %s %s\n", filepath.ToSlash(rel), target)
			return nil
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s: not a regular file", rel)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		fh := sha256.New()
		if _, err := io.Copy(fh, f); err != nil {
			return err
		}
		fmt.Fprintf(h, "F %s %t %x\n", filepath.ToSlash(rel), info.Mode()&0o111 != 0, fh.Sum(nil))
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// CheckSet checks plugins that go into one image together: unique names
// and skill names.
func CheckSet(list []*Plugin) error {
	names := map[string]bool{}
	skills := map[string]string{}
	for _, p := range list {
		if names[p.Name] {
			return fmt.Errorf("two plugins are named %s", p.Name)
		}
		names[p.Name] = true
		for _, s := range p.Skills {
			if other, ok := skills[s.Name]; ok {
				return fmt.Errorf("skill %s is in both %s and %s", s.Name, other, p.Name)
			}
			skills[s.Name] = p.Name
		}
	}
	return nil
}
