// Package spec is the build request a caller submits (Spec), its validation
// against what the builder supports (Policy), and the locked form every
// attempt of a build uses (Resolved).
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

// Base image families.
const (
	FamilyDebianSlim = "debian-slim"
	FamilyDebian     = "debian"
)

// Output types.
const (
	OutputLocal    = "local"
	OutputRegistry = "registry"
)

// Version policies.
const (
	Latest = "latest"
)

// Request is the body of POST /builds.
type Request struct {
	Spec Spec `json:"spec"`
	// NoCache builds without BuildKit's cache. It changes how, not what, so
	// it is not part of the spec digest.
	NoCache bool `json:"noCache,omitempty"`
}

// Spec is what the caller asks for. Unset fields take the defaults below.
type Spec struct {
	Base     Base     `json:"base"`
	Platform string   `json:"platform,omitempty"`
	Codex    Version  `json:"codex"`
	Plugins  []Plugin `json:"plugins,omitempty"`
	Scripts  Scripts  `json:"scripts"`
	Output   Output   `json:"output"`
}

type Base struct {
	Family  string `json:"family,omitempty"`
	Version string `json:"version,omitempty"`
}

type Version struct {
	Version string `json:"version,omitempty"`
}

// Plugin is an Agent Plugin (https://agent-plugins.org) directory in a
// git repository.
type Plugin struct {
	Source   string `json:"source"`
	Revision string `json:"revision"`
	Path     string `json:"path,omitempty"`
}

type Scripts struct {
	Build   []Script `json:"build,omitempty"`
	Startup []Script `json:"startup,omitempty"`
}

// Script runs with /bin/sh -e. Secrets names build secrets the builder's
// operator configured; each is mounted at /run/secrets/<id> for this RUN
// only.
type Script struct {
	Name    string   `json:"name"`
	Run     string   `json:"run"`
	Secrets []string `json:"secrets,omitempty"`
}

type Output struct {
	Type       string `json:"type,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// Policy is what this builder instance supports.
type Policy struct {
	// Platforms the Docker daemon builds natively; the first is the default.
	Platforms []string
	// Secrets are the build secret ids configured by the operator.
	Secrets []string
	// Registry reports whether registry output is configured.
	Registry bool
	// PluginSource reports whether a plugin source URL is allowed.
	PluginSource func(string) bool
}

// DebianReleases are the supported Debian codenames, official Docker Hub
// images.
var DebianReleases = []string{"trixie", "bookworm"}

const (
	maxPlugins     = 32
	maxScripts     = 64
	maxScriptBytes = 64 << 10
)

var (
	semver     = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	secretID   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	repository = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
)

// Normalize fills in defaults and checks s against p. The error is a
// *domain.SpecError listing every problem.
func (s *Spec) Normalize(p Policy) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if s.Base.Family == "" {
		s.Base.Family = FamilyDebianSlim
	}
	switch s.Base.Family {
	case FamilyDebianSlim, FamilyDebian:
		if s.Base.Version == "" {
			s.Base.Version = DebianReleases[0]
		}
		if !slices.Contains(DebianReleases, s.Base.Version) {
			add("base.version: %q is not one of %v", s.Base.Version, DebianReleases)
		}
	default:
		add("base.family: %q is not offered; use debian-slim or debian", s.Base.Family)
	}

	if s.Platform == "" && len(p.Platforms) > 0 {
		s.Platform = p.Platforms[0]
	}
	if !slices.Contains(p.Platforms, s.Platform) {
		add("platform: %q is not built by this builder (%v)", s.Platform, p.Platforms)
	}

	if s.Codex.Version == "" {
		s.Codex.Version = Latest
	}
	if s.Codex.Version != Latest && !semver.MatchString(s.Codex.Version) {
		add("codex.version: %q is not %q or an exact version", s.Codex.Version, Latest)
	}

	if len(s.Plugins) > maxPlugins {
		add("plugins: at most %d", maxPlugins)
	}
	seen := map[string]bool{}
	for i, pl := range s.Plugins {
		switch {
		case pl.Source == "":
			add("plugins[%d].source: required", i)
		case p.PluginSource == nil || !p.PluginSource(pl.Source):
			add("plugins[%d].source: %q is not an allowed plugin source", i, pl.Source)
		}
		if pl.Revision == "" {
			add("plugins[%d].revision: required (a commit, tag or branch; it is locked to a commit)", i)
		}
		if pl.Path != "" {
			clean := path.Clean(pl.Path)
			if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				add("plugins[%d].path: must stay inside the repository", i)
			}
			s.Plugins[i].Path = clean
		}
		key := pl.Source + "#" + pl.Revision + ":" + s.Plugins[i].Path
		if seen[key] {
			add("plugins[%d]: listed twice", i)
		}
		seen[key] = true
	}

	check := func(kind string, list []Script, secrets bool) {
		if len(list) > maxScripts {
			add("scripts.%s: at most %d", kind, maxScripts)
		}
		for i, sc := range list {
			if strings.TrimSpace(sc.Name) == "" {
				add("scripts.%s[%d].name: required", kind, i)
			}
			if strings.TrimSpace(sc.Run) == "" {
				add("scripts.%s[%d].run: required", kind, i)
			}
			if len(sc.Run) > maxScriptBytes {
				add("scripts.%s[%d].run: at most %d bytes", kind, i, maxScriptBytes)
			}
			if !secrets && len(sc.Secrets) > 0 {
				add("scripts.%s[%d].secrets: startup scripts get no build secrets; the caller provides runtime credentials", kind, i)
			}
			for _, id := range sc.Secrets {
				if !secretID.MatchString(id) || !slices.Contains(p.Secrets, id) {
					add("scripts.%s[%d].secrets: %q is not a configured build secret", kind, i, id)
				}
			}
		}
	}
	check("build", s.Scripts.Build, true)
	check("startup", s.Scripts.Startup, false)

	if s.Output.Type == "" {
		s.Output.Type = OutputLocal
	}
	switch s.Output.Type {
	case OutputLocal:
		if s.Output.Repository != "" {
			add("output.repository: only for registry output")
		}
	case OutputRegistry:
		if !p.Registry {
			add("output.type: registry output is not configured on this builder")
		}
		if !repository.MatchString(s.Output.Repository) || len(s.Output.Repository) > 200 {
			add("output.repository: %q is not a valid repository name", s.Output.Repository)
		}
	default:
		add("output.type: %q is not local or registry", s.Output.Type)
	}

	if len(problems) > 0 {
		return &domain.SpecError{Problems: problems}
	}
	return nil
}

// Resolved is a spec with every moving part locked. Attempts of one build
// all use it; only a new build resolves again.
type Resolved struct {
	// Contract is the version of the image's runtime contract (entrypoint,
	// paths, environment), see docs/runtime-contract.md.
	Contract int              `json:"contract"`
	Platform string           `json:"platform"`
	Base     ResolvedImage    `json:"base"`
	Node     ResolvedNode     `json:"node"`
	Codex    ResolvedPackage  `json:"codex"`
	Akhook   ResolvedAkhook   `json:"akhook"`
	Plugins  []ResolvedPlugin `json:"plugins"`
	Scripts  Scripts          `json:"scripts"`
	Output   Output           `json:"output"`
	// RuntimeEnv names the environment variables the image reads at run
	// time (plugin credentials and endpoints); the caller provides them.
	RuntimeEnv []string `json:"runtimeEnv"`
}

// ResolvedImage is an image pinned to its index digest.
type ResolvedImage struct {
	Family  string `json:"family,omitempty"`
	Version string `json:"version,omitempty"`
	Image   string `json:"image"`
	Digest  string `json:"digest"`
}

// Ref is the image by digest.
func (i ResolvedImage) Ref() string { return i.Image + "@" + i.Digest }

type ResolvedNode struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

type ResolvedPackage struct {
	Package string `json:"package"`
	Version string `json:"version"`
}

// ResolvedAkhook is akhook's source, built in the image from Commit with
// the Rust toolchain image Toolchain.
type ResolvedAkhook struct {
	Repository string        `json:"repository"`
	Commit     string        `json:"commit"`
	Toolchain  ResolvedImage `json:"toolchain"`
}

type ResolvedPlugin struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source"`
	Revision string `json:"revision"`
	Commit   string `json:"commit"`
	Path     string `json:"path,omitempty"`
	// SpecVersion is the Agent Plugins version the plugin declares.
	SpecVersion string   `json:"specVersion"`
	MCPServers  []string `json:"mcpServers,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Hooks       []string `json:"hooks,omitempty"`
	// Warnings report what the plugin had that was ignored or skipped
	// (unknown manifest fields, invalid skills or servers, unsupported
	// transports), as the Agent Plugins specification asks clients to.
	Warnings []string `json:"warnings,omitempty"`
}

// Digest identifies r.
func (r Resolved) Digest() string {
	b, err := Canonical(r)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Canonical encodes v as JSON with sorted object keys, so equal content
// hashes equally whatever order the client wrote it in.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&generic); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

// Hash is the sha256 of v's canonical JSON.
func Hash(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
